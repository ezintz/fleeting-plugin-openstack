// Package openstackclient wraps the gophercloud SDK behind a narrow Client
// interface covering just the compute, image and network calls the plugin
// needs, so the provider can be unit tested without a live OpenStack cloud.
package openstackclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-viper/mapstructure/v2"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/utils"
	osClient "github.com/gophercloud/utils/v2/client"
)

// AuthConfig supplies OpenStack credentials and endpoint selection. It is
// implemented by CloudConfig (clouds.yaml) and EnvCloudConfig (OS_* variables).
type AuthConfig interface {
	Parse() (gophercloud.AuthOptions, gophercloud.EndpointOpts, *tls.Config, error)
	HTTPOpts() (debug bool, computeAPIVersion string)
}

// CloudOpts holds provider-client options that are independent of how
// credentials were sourced.
type CloudOpts struct {
	AllowReauth bool `envDefault:"true"`
}

// CloudConfig selects a named cloud from a clouds.yaml file. Each field may
// also be supplied through its OS_* environment variable.
type CloudConfig struct {
	ClientConfigFile  string `json:"client-config-file" env:"OS_CLIENT_CONFIG_FILE"`
	Cloud             string `json:"cloud" env:"OS_CLOUD"`
	RegionName        string `json:"region-name" env:"OS_REGION_NAME"`
	EndpointType      string `json:"endpoint-type" env:"OS_ENDPOINT_TYPE"`
	Debug             bool   `json:"debug" env:"OS_DEBUG"`
	ComputeAPIVersion string `json:"compute-api-version" env:"OS_COMPUTE_API_VERSION" envDefault:"2.79"`
}

// EnvCloudConfig authenticates from OS_* environment variables. If Cloud is
// set it defers to the embedded CloudConfig (clouds.yaml) and only overrides
// the project scope.
type EnvCloudConfig struct {
	CloudConfig `embed:"" yaml:",inline"`

	AuthURL                     string `json:"auth-url" env:"OS_AUTH_URL"`
	Username                    string `json:"username" env:"OS_USERNAME"`
	UserID                      string `json:"user-id" env:"OS_USER_ID"`
	Password                    string `json:"password" env:"OS_PASSWORD"`
	Passcode                    string `json:"passcode" env:"OS_PASSCODE"`
	ProjectName                 string `json:"project-name" env:"OS_PROJECT_NAME"`
	ProjectID                   string `json:"project-id" env:"OS_PROJECT_ID"`
	UserDomainName              string `json:"user-domain-name" env:"OS_USER_DOMAIN_NAME"`
	UserDomainID                string `json:"user-domain-id" env:"OS_USER_DOMAIN_ID"`
	ApplicationCredentialID     string `json:"application-credential-id" env:"OS_APPLICATION_CREDENTIAL_ID"`
	ApplicationCredentialName   string `json:"application-credential-name" env:"OS_APPLICATION_CREDENTIAL_NAME"`
	ApplicationCredentialSecret string `json:"application-credential-secret" env:"OS_APPLICATION_CREDENTIAL_SECRET"`
}

// ImageProperties holds the Glance image properties the plugin uses to decide
// how to connect to an instance.
//
// See also: https://docs.openstack.org/glance/latest/admin/useful-image-properties.html
type ImageProperties struct {
	// Architecture that must be supported by the hypervisor.
	Architecture string `json:"architecture,omitempty" mapstructure:"architecture,omitempty"`

	// OSType is the operating system installed on the image.
	OSType string `json:"os_type,omitempty" mapstructure:"os_type,omitempty"`

	// OSDistro is the common name of the operating system distribution in lowercase
	OSDistro string `json:"os_distro,omitempty" mapstructure:"os_distro,omitempty"`

	// OSVersion is the operating system version as specified by the distributor.
	OSVersion string `json:"os_version,omitempty" mapstructure:"os_version,omitempty"`

	// OSAdminUser is the default admin user name for the operating system
	OSAdminUser string `json:"os_admin_user,omitempty" mapstructure:"os_admin_user,omitempty"`
}

// Client is the subset of the OpenStack API the plugin depends on. Keeping it
// an interface is what lets provider tests run against a fake.
type Client interface {
	GetImageProperties(ctx context.Context, imageRef string) (*ImageProperties, error)
	GetImageByName(ctx context.Context, imageName string) (string, *ImageProperties, error)
	GetImageByMetadata(ctx context.Context, imageIDMetadataKey string) (string, *ImageProperties, error)
	GetFlavorByName(ctx context.Context, flavorName string) (string, error)
	ShowServerConsoleOutput(ctx context.Context, serverID string) (string, error)
	GetServer(ctx context.Context, serverID string) (*servers.Server, error)
	ListServers(ctx context.Context) ([]servers.Server, error)
	CreateServer(ctx context.Context, spec servers.CreateOptsBuilder, hintOpts servers.SchedulerHintOptsBuilder) (*servers.Server, error)
	DeleteServer(ctx context.Context, serverID string) error
	CreatePort(ctx context.Context, networkID, subnetID, description string, securityGroups []string) (*ports.Port, error)
	DeletePort(ctx context.Context, portID string) error
	ListPortsByDeviceID(ctx context.Context, deviceID string) ([]ports.Port, error)
}

type client struct {
	compute *gophercloud.ServiceClient
	image   *gophercloud.ServiceClient
	network *gophercloud.ServiceClient
}

// New authenticates against OpenStack and returns a Client backed by the
// compute, image and network service endpoints.
func New(ctx context.Context, authConfig AuthConfig, cloudOpts *CloudOpts) (Client, error) {
	if cloudOpts == nil {
		cloudOpts = &CloudOpts{}
	}

	var err error
	err = env.Parse(cloudOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to parse cloudOpts: %w", err)
	}

	err = env.Parse(authConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to parse authConfig: %w", err)
	}

	providerClient, endpointOps, err := NewProviderClient(ctx, authConfig, cloudOpts)
	if err != nil {
		return nil, err
	}

	computeClient, err := NewComputeClient(ctx, providerClient, endpointOps, authConfig)
	if err != nil {
		return nil, err
	}

	imageClient, err := openstack.NewImageV2(providerClient, endpointOps)
	if err != nil {
		return nil, err
	}

	networkClient, err := openstack.NewNetworkV2(providerClient, endpointOps)
	if err != nil {
		return nil, err
	}

	return &client{
		compute: computeClient,
		image:   imageClient,
		network: networkClient,
	}, nil
}

// HTTPOpts reports the debug flag and the Nova microversion to negotiate.
func (cloudConfig *CloudConfig) HTTPOpts() (debug bool, computeAPIVersion string) {
	return cloudConfig.Debug, cloudConfig.ComputeAPIVersion
}

// Parse resolves credentials and endpoint options from the selected cloud in
// clouds.yaml, applying any region or endpoint-type override.
func (cloudConfig *CloudConfig) Parse() (gophercloud.AuthOptions, gophercloud.EndpointOpts, *tls.Config, error) {
	parseOpts := []clouds.ParseOption{clouds.WithCloudName(cloudConfig.Cloud)}
	if cloudConfig.ClientConfigFile != "" {
		parseOpts = append(parseOpts, clouds.WithLocations(cloudConfig.ClientConfigFile))
	}

	authOptions, endpointOpts, tlsCfg, err := clouds.Parse(parseOpts...)
	if err != nil {
		return gophercloud.AuthOptions{}, gophercloud.EndpointOpts{}, nil, fmt.Errorf("failed to parse clouds.yaml: %w", err)
	}

	if cloudConfig.RegionName != "" {
		endpointOpts.Region = cloudConfig.RegionName
	}
	if cloudConfig.EndpointType != "" {
		endpointOpts.Availability = gophercloud.Availability(cloudConfig.EndpointType)
	}

	return authOptions, endpointOpts, tlsCfg, nil
}

// Parse resolves credentials from OS_* environment variables, or from
// clouds.yaml when a cloud name is set.
func (envCloudConfig *EnvCloudConfig) Parse() (gophercloud.AuthOptions, gophercloud.EndpointOpts, *tls.Config, error) {
	if envCloudConfig.Cloud != "" {
		authOptions, endpointOpts, tlsCfg, err := envCloudConfig.CloudConfig.Parse()
		if err != nil {
			return gophercloud.AuthOptions{}, gophercloud.EndpointOpts{}, nil, err
		}

		if envCloudConfig.ProjectName != "" {
			authOptions.TenantName = envCloudConfig.ProjectName
			authOptions.TenantID = ""
		}
		if envCloudConfig.ProjectID != "" {
			authOptions.TenantID = envCloudConfig.ProjectID
			authOptions.TenantName = ""
		}

		return authOptions, endpointOpts, tlsCfg, nil
	}

	authOptions := gophercloud.AuthOptions{
		IdentityEndpoint:            envCloudConfig.AuthURL,
		UserID:                      envCloudConfig.UserID,
		Username:                    envCloudConfig.Username,
		Password:                    envCloudConfig.Password,
		Passcode:                    envCloudConfig.Passcode,
		TenantID:                    envCloudConfig.ProjectID,
		TenantName:                  envCloudConfig.ProjectName,
		DomainID:                    envCloudConfig.UserDomainID,
		DomainName:                  envCloudConfig.UserDomainName,
		ApplicationCredentialID:     envCloudConfig.ApplicationCredentialID,
		ApplicationCredentialName:   envCloudConfig.ApplicationCredentialName,
		ApplicationCredentialSecret: envCloudConfig.ApplicationCredentialSecret,
	}

	endpointOpts := gophercloud.EndpointOpts{
		Region:       envCloudConfig.RegionName,
		Availability: gophercloud.Availability(envCloudConfig.EndpointType),
	}

	return authOptions, endpointOpts, nil, nil
}

// NewHTTPClient builds the HTTP client used for all OpenStack API calls,
// applying the TLS configuration from clouds.yaml and wrapping the transport
// so gophercloud's request logging is available.
func NewHTTPClient(tlsCfg *tls.Config) http.Client {
	// http.DefaultTransport is always *http.Transport in the standard
	// library, but it is a package-level var that any imported package can
	// reassign. Fall back to a fresh transport rather than panicking inside
	// a long-running runner process.
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = new(http.Transport)
	}

	transport := base.Clone()
	if tlsCfg != nil {
		transport.TLSClientConfig = tlsCfg
	}

	return http.Client{
		Transport: &osClient.RoundTripper{Rt: transport},
	}
}

// NewProviderClient authenticates and returns the shared provider client all
// service clients are derived from.
func NewProviderClient(ctx context.Context, authConfig AuthConfig, cloudOpts *CloudOpts) (*gophercloud.ProviderClient, gophercloud.EndpointOpts, error) {
	authOptions, endpointOpts, tlsCfg, err := authConfig.Parse()
	if err != nil {
		return nil, gophercloud.EndpointOpts{}, err
	}

	httpClient := NewHTTPClient(tlsCfg)
	authOptions.AllowReauth = cloudOpts.AllowReauth

	providerClient, err := config.NewProviderClient(ctx, authOptions, config.WithHTTPClient(httpClient))
	if err != nil {
		return nil, gophercloud.EndpointOpts{}, err
	}

	return providerClient, endpointOpts, nil
}

// NewComputeClient returns a Nova client pinned to the configured
// microversion, which the plugin needs for tags and block device mappings.
func NewComputeClient(ctx context.Context, providerClient *gophercloud.ProviderClient, endpointOps gophercloud.EndpointOpts, authConfig AuthConfig) (*gophercloud.ServiceClient, error) {
	_, computeAPIVersion := authConfig.HTTPOpts()

	computeClient, err := openstack.NewComputeV2(providerClient, endpointOps)
	if err != nil {
		return &gophercloud.ServiceClient{}, err
	}

	_computeClient, err := utils.RequireMicroversion(ctx, *computeClient, computeAPIVersion)
	if err != nil {
		return &gophercloud.ServiceClient{}, err
	}

	return &_computeClient, err
}

func (c *client) GetImageProperties(ctx context.Context, imageRef string) (*ImageProperties, error) {
	image, err := images.Get(ctx, c.image, imageRef).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to get image %s: %w", imageRef, err)
	}

	out := new(ImageProperties)
	err = mapstructure.Decode(image.Properties, out)
	if err != nil {
		return nil, fmt.Errorf("failed to parse properties: %w", err)
	}

	return out, nil
}

func (c *client) GetImageByName(ctx context.Context, imageName string) (string, *ImageProperties, error) {
	page, err := images.List(c.image, images.ListOpts{Name: imageName}).AllPages(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("failed to list images: %w", err)
	}

	imgs, err := images.ExtractImages(page)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse images: %w", err)
	}

	if len(imgs) == 0 {
		err = gophercloud.ErrResourceNotFound{Name: imageName, ResourceType: "image"}
		return "", nil, err
	} else if len(imgs) > 1 {
		err = gophercloud.ErrMultipleResourcesFound{Name: imageName, Count: len(imgs), ResourceType: "image"}
		return "", nil, err
	}

	out := new(ImageProperties)
	err = mapstructure.Decode(imgs[0].Properties, out)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse properties: %w", err)
	}

	return imgs[0].ID, out, nil
}

func (c *client) GetImageByMetadata(ctx context.Context, imageRefMetadataKey string) (string, *ImageProperties, error) {
	http := http.Client{Timeout: 10 * time.Second}
	response, err := http.Get("http://169.254.169.254/openstack/latest/meta_data.json")
	if err != nil {
		return "", nil, fmt.Errorf("failed to query metadata service: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	var metadata struct {
		Meta map[string]string `json:"meta,omitempty"`
	}
	err = json.NewDecoder(response.Body).Decode(&metadata)
	if err != nil {
		return "", nil, fmt.Errorf("failed to decode metadata json: %w", err)
	}

	imageID, ok := metadata.Meta[imageRefMetadataKey]
	if !ok {
		return "", nil, fmt.Errorf("the metadata does not contain an entry with the name: %s", imageRefMetadataKey)
	}

	image, err := images.Get(ctx, c.image, imageID).Extract()
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse image: %w", err)
	}

	out := new(ImageProperties)
	err = mapstructure.Decode(image.Properties, out)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse properties: %w", err)
	}

	return image.ID, out, nil
}

func (c *client) GetFlavorByName(ctx context.Context, flavorName string) (string, error) {
	page, err := flavors.ListDetail(c.compute, flavors.ListOpts{AccessType: flavors.PublicAccess}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list flavors: %w", err)
	}

	flvs, err := flavors.ExtractFlavors(page)
	if err != nil {
		return "", fmt.Errorf("failed to parse flavors: %w", err)
	}

	flvs = slices.DeleteFunc(flvs, func(flv flavors.Flavor) bool {
		return flv.Name != flavorName
	})

	if len(flvs) == 0 {
		err = gophercloud.ErrResourceNotFound{Name: flavorName, ResourceType: "flavor"}
		return "", err
	} else if len(flvs) > 1 {
		err = gophercloud.ErrMultipleResourcesFound{Name: flavorName, Count: len(flvs), ResourceType: "flavor"}
		return "", err
	}

	return flvs[0].ID, nil
}

func (c *client) ShowServerConsoleOutput(ctx context.Context, serverID string) (string, error) {
	return servers.ShowConsoleOutput(ctx, c.compute, serverID, servers.ShowConsoleOutputOpts{
		Length: 100,
	}).Extract()
}

func (c *client) GetServer(ctx context.Context, serverID string) (*servers.Server, error) {
	return servers.Get(ctx, c.compute, serverID).Extract()
}

func (c *client) ListServers(ctx context.Context) ([]servers.Server, error) {
	page, err := servers.List(c.compute, nil).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("server listing error: %w", err)
	}

	allServers, err := servers.ExtractServers(page)
	if err != nil {
		return nil, fmt.Errorf("server listing extract error: %w", err)
	}

	return allServers, nil
}

func (c *client) CreateServer(ctx context.Context, spec servers.CreateOptsBuilder, hintOpts servers.SchedulerHintOptsBuilder) (*servers.Server, error) {
	return servers.Create(ctx, c.compute, spec, hintOpts).Extract()
}

func (c *client) DeleteServer(ctx context.Context, serverID string) error {
	return servers.Delete(ctx, c.compute, serverID).ExtractErr()
}

func (c *client) CreatePort(ctx context.Context, networkID, subnetID, description string, securityGroups []string) (*ports.Port, error) {
	opts := ports.CreateOpts{
		NetworkID:   networkID,
		Description: description,
		FixedIPs: []ports.IP{
			{SubnetID: subnetID},
		},
	}
	// Nova only applies server_spec.security_groups to ports it creates
	// itself. When the plugin pre-creates a port (subnet_id flow), the
	// security groups must be attached at port-creation time, otherwise
	// the worker boots with just the tenant default and the configured
	// groups are silently ignored.
	if len(securityGroups) > 0 {
		sg := make([]string, len(securityGroups))
		copy(sg, securityGroups)
		opts.SecurityGroups = &sg
	}
	return ports.Create(ctx, c.network, opts).Extract()
}

func (c *client) DeletePort(ctx context.Context, portID string) error {
	return ports.Delete(ctx, c.network, portID).ExtractErr()
}

func (c *client) ListPortsByDeviceID(ctx context.Context, deviceID string) ([]ports.Port, error) {
	page, err := ports.List(c.network, ports.ListOpts{DeviceID: deviceID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("port listing error: %w", err)
	}

	allPorts, err := ports.ExtractPorts(page)
	if err != nil {
		return nil, fmt.Errorf("port listing extract error: %w", err)
	}

	return allPorts, nil
}
