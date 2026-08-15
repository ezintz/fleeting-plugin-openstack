package fpoc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ezintz/fleeting-plugin-openstack/internal/openstackclient"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/go-hclog"
	"github.com/jinzhu/copier"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// MetadataKey is the Nova server metadata key that marks an instance as
// belonging to a particular instance group. Its value is InstanceGroup.Name,
// and it is how the plugin distinguishes its own workers from every other
// server in the project.
const MetadataKey = "fleeting-cluster"

// PortDescription is written to the description field of every Neutron port
// the plugin pre-creates for a networks[].subnet_id entry, so those ports can
// be identified and cleaned up when the instance is deleted.
const PortDescription = "fleeting-plugin-openstack"

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

// InstanceGroup is the fleeting provider for OpenStack. Exported fields are
// unmarshalled from the runner's [runners.autoscaler.plugin_config] block, so
// their json tags form the plugin's configuration schema.
type InstanceGroup struct {
	Cloud            string        `json:"cloud"`             // cloud to use
	CloudsConfig     string        `json:"clouds_config"`     // optional: path to clouds.yaml
	Name             string        `json:"name"`              // name of the cluster
	NovaMicroversion string        `json:"nova_microversion"` // Microversion for the Nova client
	ServerSpec       ExtCreateOpts `json:"server_spec"`       // instance creation spec
	UseIgnition      bool          `json:"use_ignition"`      // Configure keys via Ignition (Fedora CoreOS / Flatcar)
	BootTimeS        string        `json:"boot_time"`         // optional: wait some time before report machine as available
	BootTime         time.Duration
	VolumeType       string `json:"volume_type"`
	VolumeSize       int    `json:"volume_size"`

	client          openstackclient.Client
	settings        provider.Settings
	log             hclog.Logger
	imgProps        atomic.Pointer[openstackclient.ImageProperties]
	sshPubKey       string
	instanceCounter atomic.Int32
}

// Init connects to OpenStack, validates the configured server spec, and
// prepares credentials. In Ignition mode it generates the SSH keypair that
// will be injected into each instance.
func (g *InstanceGroup) Init(ctx context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	g.log = log.With("name", g.Name, "cloud", g.Cloud)
	g.log.Debug("Initializing fleeting-plugin-openstack")

	var err error
	g.client, err = openstackclient.New(ctx, &openstackclient.EnvCloudConfig{
		CloudConfig: openstackclient.CloudConfig{
			ClientConfigFile:  g.CloudsConfig,
			Cloud:             g.Cloud,
			ComputeAPIVersion: g.NovaMicroversion,
		},
	}, nil)

	if err != nil {
		return provider.ProviderInfo{}, err
	}

	_, err = g.ServerSpec.ToServerCreateMap()
	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("failed to check server_spec: %w", err)
	}

	if err := validateNameTemplate(g.ServerSpec.Name); err != nil {
		return provider.ProviderInfo{}, err
	}

	if g.ServerSpec.ImageRef != "" {
		imgProps, err := g.client.GetImageProperties(ctx, g.ServerSpec.ImageRef)
		if err != nil {
			return provider.ProviderInfo{}, err
		}

		g.imgProps.Store(imgProps)
	}

	if g.VolumeSize != 0 && g.VolumeType != "" {
		if slices.ContainsFunc(g.ServerSpec.BlockDevice, func(blockDevice servers.BlockDevice) bool {
			return blockDevice.BootIndex == 0
		}) {
			return provider.ProviderInfo{}, fmt.Errorf("volume_size and volume_type can only be specified if there is no server_spec.block_device_mapping_v2 with boot_index set to 0")
		}
	}

	if !g.UseIgnition && !settings.UseStaticCredentials {
		return provider.ProviderInfo{}, fmt.Errorf("only static credentials supported in Cloud-Init mode")
	}

	if g.UseIgnition {
		err = g.initSSHKey(ctx, log, &settings)
		if err != nil {
			return provider.ProviderInfo{}, err
		}
	}

	if g.BootTimeS != "" {
		g.BootTime, err = time.ParseDuration(g.BootTimeS)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("failed to parse boot_time: %w", err)
		}
	}

	g.settings = settings
	if _, err := g.getInstances(ctx); err != nil {
		return provider.ProviderInfo{}, err
	}

	return provider.ProviderInfo{
		ID:        path.Join("openstack", g.Cloud, g.Name),
		MaxSize:   1000,
		Version:   Version,
		BuildInfo: BuildInfo(),
	}, nil
}

// Update reports the current state of every instance in the group. An ACTIVE
// server is only promoted to StateRunning once boot_time has elapsed or its
// console output shows cloud-init/Ignition has finished, so the runner does
// not dispatch jobs to a machine that is not ready.
func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	instances, err := g.getInstances(ctx)
	if err != nil {
		return err
	}

	var reterr error
	for _, srv := range instances {
		state := provider.StateCreating
		lg := g.log.With("server_id", srv.ID, "created", srv.Created, "status", srv.Status)

		switch srv.Status {
		case "BUILD", "MIGRATING", "PAUSED", "REBUILD":
			// pass

		case "DELETED", "SHUTOFF", "UNKNOWN", "ERROR":
			// Nova has either given up on this instance (ERROR) or it will
			// never come back to a usable state on its own (SHUTOFF,
			// UNKNOWN, and DELETED — the last can still show up briefly
			// here since ListServers doesn't guarantee it's gone yet).
			//
			// The runner never asks us to remove an instance on its own
			// initiative: Decrease is only called for instances an operator
			// explicitly requested removal of via the executor. Reporting
			// StateTimeout here (as this used to) doesn't fix that either —
			// the provisioner just stops tracking the instance without ever
			// calling Decrease. Either way, without deleting it ourselves,
			// a broken instance sits in the tenant, consuming quota and
			// cost, forever. So the plugin cleans it up directly, the same
			// way Decrease would.
			if srv.Status == "ERROR" {
				lg.Warn("Instance is in ERROR state; deleting it")
			}

			if delErr := g.deleteInstance(ctx, srv.ID); delErr != nil {
				lg.Error("Failed to delete broken instance", "err", delErr)
				reterr = errors.Join(reterr, delErr)
			}

			state = provider.StateDeleting

		case "ACTIVE":
			if srv.Created.Add(g.BootTime).Before(time.Now()) {
				// treat all nodes running long enough as Running
				state = provider.StateRunning
			} else {
				log, err := g.client.ShowServerConsoleOutput(ctx, srv.ID)
				if err != nil {
					reterr = errors.Join(reterr, err)
					continue
				}

				switch {
				case !g.UseIgnition && IsCloudInitFinished(log):
					lg.Info("Instance cloud-init finished")
					state = provider.StateRunning

				case g.UseIgnition && IsIgnitionFinished(log):
					lg.Info("Instance ignition finished")
					state = provider.StateRunning

				default:
					lg.Debug("Instance boot time not passed and cloud-init/ignition not finished", "boot_time", g.BootTime)
				}
			}
		}

		update(srv.ID, state)
	}

	return reterr
}

// Increase requests delta additional instances and reports how many creation
// requests OpenStack accepted. Failures are logged and joined into err, but do
// not abort the remaining requests.
func (g *InstanceGroup) Increase(ctx context.Context, delta int) (succeeded int, err error) {
	for idx := 0; idx < delta; idx++ {
		id, err2 := g.createInstance(ctx)
		if err2 != nil {
			g.log.Error("Failed to create instance", "err", err2)
			err = errors.Join(err, err2)
		} else {
			g.log.Info("Instance creation request successful", "id", id)
			succeeded++
		}
	}

	g.log.Info("Increase", "delta", delta, "succeeded", succeeded)

	return
}

// Decrease deletes the named instances and returns those whose deletion was
// accepted. Ports the plugin pre-created for a subnet_id are looked up before
// the server is deleted and removed afterwards.
func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) (succeeded []string, err error) {
	if len(instances) == 0 {
		return nil, nil
	}

	succeeded = make([]string, 0, len(instances))
	for _, id := range instances {
		if err2 := g.deleteInstance(ctx, id); err2 != nil {
			g.log.Error("Failed to delete instance", "err", err2, "id", id)
			err = errors.Join(err, err2)
			continue
		}

		g.log.Info("Instance deletion request successful", "id", id)
		succeeded = append(succeeded, id)
	}

	g.log.Info("Decrease", "instances", instances)

	return succeeded, err
}

// deleteInstance deletes id and cleans up its pre-created ports (matching
// PortDescription). Port lookup happens before server deletion since
// ListPortsByDeviceID needs the still-existing device. A 404 from
// DeleteServer means id is already gone, which is treated as success so
// callers don't have to special-case it.
//
// Shared by Decrease (runner-requested removal) and Update (the plugin's
// own cleanup of instances Nova has abandoned; see the ERROR/SHUTOFF/
// UNKNOWN case there).
func (g *InstanceGroup) deleteInstance(ctx context.Context, id string) error {
	// Collect pre-created ports (identified by description) before
	// deleting the server, so we can clean them up afterwards.
	var portIDs []string
	serverPorts, listErr := g.client.ListPortsByDeviceID(ctx, id)
	if listErr != nil {
		g.log.Warn("Failed to list ports for server", "id", id, "err", listErr)
	} else {
		for _, p := range serverPorts {
			if p.Description == PortDescription {
				portIDs = append(portIDs, p.ID)
			}
		}
	}

	if err := g.client.DeleteServer(ctx, id); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return err
	}

	g.cleanupPorts(ctx, portIDs)

	return nil
}

func (g *InstanceGroup) getInstances(ctx context.Context) ([]servers.Server, error) {
	allServers, err := g.client.ListServers(ctx)
	if err != nil {
		return nil, err
	}

	filteredServers := make([]servers.Server, 0, len(allServers))
	for _, srv := range allServers {
		cluster, ok := srv.Metadata[MetadataKey]
		if !ok || cluster != g.Name {
			continue
		}

		filteredServers = append(filteredServers, srv)
	}

	return filteredServers, nil
}

// validateNameTemplate rejects a server_spec.name that createInstance cannot
// expand into a unique per-instance name.
//
// createInstance runs the name through fmt.Sprintf with the group's instance
// counter, so it has to carry exactly one integer verb. Misuse is not a hard
// failure — fmt reports it inline instead, so a template like "runner" turns
// into the literal server name "runner%!(EXTRA int=1)" and every instance in
// the group carries that marker for its whole life. Catch it at Init, where
// the operator still sees the error, rather than at the Nova API.
func validateNameTemplate(name string) error {
	// Any misuse of the template — no verb, a non-integer verb, or more than
	// one — shows up as a %!-prefixed error marker in the expansion.
	if expanded := fmt.Sprintf(name, 1); strings.Contains(expanded, "%!") {
		return fmt.Errorf(
			"server_spec.name %q must contain exactly one integer format verb (for example %q) so each instance gets a unique name, but expanding it produced %q",
			name, "runner-%d", expanded)
	}

	return nil
}

func (g *InstanceGroup) createInstance(ctx context.Context) (string, error) {
	spec := new(ExtCreateOpts)
	err := copier.Copy(spec, &g.ServerSpec)
	if err != nil {
		return "", err
	}

	index := int(g.instanceCounter.Add(1))

	spec.Name = fmt.Sprintf(g.ServerSpec.Name, index)
	if spec.Metadata == nil {
		spec.Metadata = make(map[string]string)
	}
	spec.Metadata[MetadataKey] = g.Name

	var hintOpts servers.SchedulerHintOptsBuilder
	if spec.SchedulerHints != nil {
		hintOpts = spec.SchedulerHints
	}

	if spec.ImageName != "" {
		imageRef, imgProps, err := g.client.GetImageByName(ctx, spec.ImageName)
		if err != nil {
			return "", err
		}

		spec.ImageRef = imageRef
		g.imgProps.Store(imgProps)

		g.log.Debug("Image resolved by name", "image_name", spec.ImageName, "image_ref", spec.ImageRef)
	} else if spec.ImageRefFromMetadata != "" {
		imageRef, imgProps, err := g.client.GetImageByMetadata(ctx, spec.ImageRefFromMetadata)
		if err != nil {
			return "", err
		}

		spec.ImageRef = imageRef
		g.imgProps.Store(imgProps)

		g.log.Debug("Image resolved by metadata", "image_name", spec.ImageName, "image_ref", spec.ImageRef)
	}

	if spec.FlavorName != "" {
		flavorRef, err := g.client.GetFlavorByName(ctx, spec.FlavorName)
		if err != nil {
			return "", err
		}

		spec.FlavorRef = flavorRef

		g.log.Debug("Flavor resolved by name", "flavor_name", spec.FlavorName, "flavor_ref", spec.FlavorRef)
	}

	if g.UseIgnition {
		username, err := g.resolveUsername()
		if err != nil {
			return "", err
		}

		err = InsertSSHKeyIgn(spec, username, g.sshPubKey)
		if err != nil {
			return "", err
		}
	}

	// Pre-create Neutron ports for networks that specify a SubnetID.
	// Nova does not support subnet_id directly, so we create a port on the
	// desired subnet and pass the port ID to Nova instead.
	//
	// Build a fresh networks slice rather than mutating spec.Networks in
	// place: copier.Copy gives a shallow copy, so spec.Networks shares
	// backing storage with g.ServerSpec.Networks. Mutating it would leak
	// per-instance port IDs into the source spec and the next
	// createInstance call would resubmit the stale port.
	var createdPortIDs []string
	networks := make([]PluginNetwork, len(spec.Networks))
	copy(networks, spec.Networks)
	for i, net := range networks {
		if net.SubnetID == "" {
			continue
		}

		port, err := g.client.CreatePort(ctx, net.UUID, net.SubnetID, PortDescription, spec.SecurityGroups)
		if err != nil {
			g.cleanupPorts(ctx, createdPortIDs)
			return "", fmt.Errorf("failed to create port for subnet %s: %w", net.SubnetID, err)
		}

		createdPortIDs = append(createdPortIDs, port.ID)
		g.log.Debug("Pre-created port for subnet", "port_id", port.ID, "network_id", net.UUID, "subnet_id", net.SubnetID)

		networks[i] = PluginNetwork{Port: port.ID, Tag: net.Tag}
	}
	spec.Networks = networks

	if g.VolumeSize != 0 && g.VolumeType != "" {
		spec.BlockDevice = append(spec.BlockDevice, servers.BlockDevice{
			BootIndex:           0,
			DeleteOnTermination: true,
			DestinationType:     servers.DestinationVolume,
			SourceType:          servers.SourceImage,
			UUID:                spec.ImageRef,
			VolumeSize:          g.VolumeSize,
			VolumeType:          g.VolumeType,
		})
	}

	srv, err := g.client.CreateServer(ctx, spec, hintOpts)
	if err != nil {
		g.cleanupPorts(ctx, createdPortIDs)
		return "", err
	}

	return srv.ID, nil
}

// resolveUsername returns the account instances in this group are reached
// as: the operator-supplied connector_config.username if set, otherwise the
// image's os_admin_user property.
//
// createInstance (which authorizes the plugin's key for this account via
// Ignition) and ConnectInfo (which hands the account to the SSH connector)
// must agree. Resolving in one place keeps them from drifting: previously
// only createInstance applied the os_admin_user fallback, so ConnectInfo
// reported an empty username and every SSH connection failed.
func (g *InstanceGroup) resolveUsername() (string, error) {
	if g.settings.Username != "" {
		return g.settings.Username, nil
	}

	// Deliberately no built-in default: unlike EC2 ("ec2-user") or Azure
	// ("azureuser"), OpenStack images have no conventional admin account
	// (core, fedora, ubuntu, cloud-user, ...), so guessing would fail at
	// SSH time with a far less obvious error than this one.
	if imgProps := g.imgProps.Load(); imgProps != nil && imgProps.OSAdminUser != "" {
		return imgProps.OSAdminUser, nil
	}

	return "", fmt.Errorf("image properties 'os_admin_user' and 'runners.autoscaler.connector_config.username' missing; ensure one is set")
}

// cleanupPorts deletes a list of pre-created ports, logging any errors.
// portCleanupTimeout bounds how long cleanupPorts retries a single port
// against Nova's asynchronous server teardown (see deletePortRetryingConflict).
const portCleanupTimeout = 30 * time.Second

func (g *InstanceGroup) cleanupPorts(ctx context.Context, portIDs []string) {
	for _, portID := range portIDs {
		if err := g.deletePortRetryingConflict(ctx, portID); err != nil {
			g.log.Error("Failed to clean up port", "port_id", portID, "err", err)
		}
	}
}

// deletePortRetryingConflict deletes a pre-created port, retrying while
// Neutron reports it as still owned by a device.
//
// DELETE /servers/{id} answers before the server is actually gone: Nova
// marks it "deleting" and detaches its ports in the background. A port we
// pre-created and handed to Nova at boot is still owned by that
// still-deleting server for a window after the call returns, so an
// immediate DeletePort races that teardown and fails with 409 Conflict.
// Poll until Neutron releases the port, treating 404 (already gone — Nova's
// own cleanup or a previous attempt beat us to it) as success.
func (g *InstanceGroup) deletePortRetryingConflict(ctx context.Context, portID string) error {
	waitCtx, cancel := context.WithTimeout(ctx, portCleanupTimeout)
	defer cancel()

	return gophercloud.WaitFor(waitCtx, func(ctx context.Context) (bool, error) {
		err := g.client.DeletePort(ctx, portID)
		switch {
		case err == nil:
			return true, nil
		case gophercloud.ResponseCodeIs(err, http.StatusNotFound):
			return true, nil
		case gophercloud.ResponseCodeIs(err, http.StatusConflict):
			return false, nil
		default:
			return true, err
		}
	})
}

// ConnectInfo returns the address, account and credentials the runner needs to
// open a connection to an instance. OS and architecture are taken from the
// image properties captured when the image was resolved.
func (g *InstanceGroup) ConnectInfo(ctx context.Context, instanceID string) (provider.ConnectInfo, error) {
	srv, err := g.client.GetServer(ctx, instanceID)
	if err != nil {
		return provider.ConnectInfo{}, fmt.Errorf("failed to get server %s: %w", instanceID, err)
	}

	if srv.Status != "ACTIVE" {
		return provider.ConnectInfo{}, fmt.Errorf("instance status is not active: %s", srv.Status)
	}

	ipAddr := srv.AccessIPv4
	if ipAddr == "" {
		netAddrs, err := extractAddresses(srv)
		if err != nil {
			return provider.ConnectInfo{}, err
		}

		// TODO: detect internal (tenant) and external networks
		for net, addrs := range netAddrs {
			for _, addr := range addrs {
				ipAddr = addr.Address
				g.log.Debug("Use address", "network", net, "ip_address", ipAddr)
			}
		}
	}

	info := provider.ConnectInfo{
		ConnectorConfig: g.settings.ConnectorConfig,
		ID:              instanceID,
		InternalAddr:    ipAddr,
		ExternalAddr:    ipAddr,
	}
	info.Protocol = provider.ProtocolSSH

	// The connector passes this straight to ssh.ClientConfig.User, so an
	// empty value fails authentication rather than defaulting to anything.
	info.Username, err = g.resolveUsername()
	if err != nil {
		return provider.ConnectInfo{}, err
	}

	imgProps := g.imgProps.Load()

	// XXX TODO: srv.Image in many conditions may be empty, so you should go and check volume meta.
	//           but for simplicity we just keep the last image and assume the props we want keeps the same...
	// if imgProps == nil && srv.Image != nil {
	// 	image := new(images.Image)
	// 	err = mapstructure.Decode(srv.Image, image)
	// 	if err != nil {
	// 		return provider.ConnectInfo{}, err
	// 	}
	//
	// 	imgProps, err = g.client.GetImageProperties(ctx, image.ID)
	// 	if err != nil {
	// 		return provider.ConnectInfo{}, err
	// 	}
	// }

	if imgProps != nil {
		switch imgProps.OSType {
		case "", "linux":
			info.Protocol = provider.ProtocolSSH
			info.OS = "linux"

		case "windows":
			g.log.Warn("Windows not really supported by the plugin.")
			info.Protocol = provider.ProtocolWinRM
			info.OS = imgProps.OSType

		default:
			g.log.Warn("Unknown image os_type", "os_type", imgProps.OSType)
			info.OS = imgProps.OSType
		}

		switch imgProps.Architecture {
		case "", "x86_64":
			info.Arch = "amd64"

		case "aarch64":
			info.Arch = "arm64"

		default:
			g.log.Warn("Unknown image arch", "arch", imgProps.Architecture)
		}
	} else {
		// default to linux on amd64
		info.OS = "linux"
		info.Arch = "amd64"
	}

	return info, nil
}

// Heartbeat reports whether instanceID is still a usable Nova instance. The
// runner calls this before dispatching work to an instance it already
// believes is running, so it deliberately checks only server status —
// nothing as slow as an SSH probe.
func (g *InstanceGroup) Heartbeat(ctx context.Context, instanceID string) error {
	srv, err := g.client.GetServer(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to get server %s: %w", instanceID, err)
	}

	if srv.Status != "ACTIVE" {
		return fmt.Errorf("%w: status is %s", provider.ErrInstanceUnhealthy, srv.Status)
	}

	return nil
}

// Suspend is unimplemented: Init never advertises
// provider.CapabilitySuspendResume, so the runner's provisioner never calls
// this — it only forwards Suspend/Resume requests to plugins that opted in.
func (g *InstanceGroup) Suspend(_ context.Context, _ []string) (succeeded []string, err error) {
	return nil, provider.ErrSuspendResumeNotSupported
}

// Resume is unimplemented; see Suspend.
func (g *InstanceGroup) Resume(_ context.Context, _ []string) (succeeded []string, err error) {
	return nil, provider.ErrSuspendResumeNotSupported
}

// Shutdown is a no-op: instances outlive the plugin process and are reclaimed
// by the runner through Decrease, not at shutdown.
func (g *InstanceGroup) Shutdown(_ context.Context) error {
	return nil
}
