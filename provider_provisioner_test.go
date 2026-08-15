package fpoc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gitlab.com/gitlab-org/fleeting/fleeting"
	"gitlab.com/gitlab-org/fleeting/fleeting/integration"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/ezintz/fleeting-plugin-openstack/internal/openstackclient"
)

// tenant is a scripted stand-in for a Nova/Neutron tenant: servers created
// through it start in BUILD and reach ACTIVE after buildTime, the way a real
// one does, so the fleeting Provisioner has to poll them through the same
// state machine it would against a real cloud.
type tenant struct {
	mu        sync.Mutex
	servers   map[string]*servers.Server
	next      int
	buildTime time.Duration
	console   string

	// undeletable, when non-empty, names a server whose DeleteServer always
	// fails: the locked-server / policy-denial case.
	undeletable string
	deleteErr   error
}

func newTenant() *tenant {
	return &tenant{
		servers:   map[string]*servers.Server{},
		buildTime: 300 * time.Millisecond,
		console:   "Cloud-init v. 23.1.2 finished at Fri, 15 Aug 2026 21:00:00 +0000. Up 12.00 seconds\n",
	}
}

func (c *tenant) inject(id, status string, meta map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.servers[id] = &servers.Server{ID: id, Status: status, Metadata: meta, Created: time.Now()}
}

func (c *tenant) CreateServer(_ context.Context, spec servers.CreateOptsBuilder, _ servers.SchedulerHintOptsBuilder) (*servers.Server, error) {
	sm, err := spec.ToServerCreateMap()
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.next++
	id := fmt.Sprintf("srv-%d", c.next)

	// gophercloud builds the request body by reflection, so metadata comes
	// back as map[string]any, not map[string]string.
	body, _ := sm["server"].(map[string]any)
	meta := map[string]string{}

	switch m := body["metadata"].(type) {
	case map[string]string:
		meta = m
	case map[string]any:
		for k, val := range m {
			meta[k] = fmt.Sprint(val)
		}
	}

	c.servers[id] = &servers.Server{
		ID: id, Status: "BUILD", Metadata: meta, Created: time.Now(),
		Addresses: map[string]any{
			"tenant-net": []any{
				map[string]any{"addr": fmt.Sprintf("10.20.0.%d", c.next), "version": float64(4), "OS-EXT-IPS:type": "fixed"},
				map[string]any{"addr": fmt.Sprintf("203.0.113.%d", c.next), "version": float64(4), "OS-EXT-IPS:type": "floating"},
			},
		},
	}

	return &servers.Server{ID: id}, nil
}

func (c *tenant) ListServers(_ context.Context) ([]servers.Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]servers.Server, 0, len(c.servers))
	for _, s := range c.servers {
		if s.Status == "BUILD" && time.Since(s.Created) > c.buildTime {
			s.Status = "ACTIVE"
		}

		out = append(out, *s)
	}

	return out, nil
}

func (c *tenant) GetServer(_ context.Context, id string) (*servers.Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	s, ok := c.servers[id]
	if !ok {
		return nil, errors.New("server not found")
	}

	srv := *s

	return &srv, nil
}

func (c *tenant) DeleteServer(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if id == c.undeletable {
		return c.deleteErr
	}

	delete(c.servers, id)

	return nil
}

func (c *tenant) ShowServerConsoleOutput(_ context.Context, _ string) (string, error) {
	return c.console, nil
}

func (c *tenant) GetImageProperties(_ context.Context, _ string) (*openstackclient.ImageProperties, error) {
	return verifyImageProps(), nil
}

func (c *tenant) GetImageByName(_ context.Context, _ string) (string, *openstackclient.ImageProperties, error) {
	return "image-uuid", verifyImageProps(), nil
}

func (c *tenant) GetImageByMetadata(_ context.Context, _ string) (string, *openstackclient.ImageProperties, error) {
	return "image-uuid", verifyImageProps(), nil
}

func (c *tenant) GetFlavorByName(_ context.Context, _ string) (string, error) {
	return "flavor-uuid", nil
}

func (c *tenant) CreatePort(_ context.Context, networkID, _, description string, _ []string) (*ports.Port, error) {
	return &ports.Port{ID: "port-1", NetworkID: networkID, Description: description}, nil
}

func (c *tenant) DeletePort(_ context.Context, _ string) error { return nil }

func (c *tenant) ListPortsByDeviceID(_ context.Context, _ string) ([]ports.Port, error) {
	return nil, nil
}

func verifyImageProps() *openstackclient.ImageProperties {
	return &openstackclient.ImageProperties{OSType: "linux", Architecture: "x86_64"}
}

// verifyGroup is the real InstanceGroup with the one thing this test cannot
// supply replaced: Init's authentication against a live cloud. It wires in
// the scripted tenant and the state Init would otherwise have left behind, so
// every method the Provisioner calls afterwards — Update, Increase, Decrease,
// ConnectInfo — is the production code path. TestProvisioning covers the real
// Init against a real cloud.
type verifyGroup struct {
	*InstanceGroup

	tenant *tenant
}

func (v *verifyGroup) Init(ctx context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	g := v.InstanceGroup

	g.log = log.With("name", g.Name)
	g.client = v.tenant
	g.settings = settings
	g.imgProps.Store(verifyImageProps())

	if _, err := g.ServerSpec.ToServerCreateMap(); err != nil {
		return provider.ProviderInfo{}, err
	}

	if err := validateNameTemplate(g.ServerSpec.Name); err != nil {
		return provider.ProviderInfo{}, err
	}

	if _, err := g.getInstances(ctx); err != nil {
		return provider.ProviderInfo{}, err
	}

	return provider.ProviderInfo{
		ID:        "openstack/verify/" + g.Name,
		MaxSize:   1000,
		Version:   Version,
		BuildInfo: BuildInfo(),
	}, nil
}

func newVerifyGroup(c *tenant) *verifyGroup {
	return &verifyGroup{
		tenant: c,
		InstanceGroup: &InstanceGroup{
			Name: "prod-verify",
			ServerSpec: ExtCreateOpts{
				CreateOpts: servers.CreateOpts{
					Name:      "runner-%d",
					ImageRef:  "image-uuid",
					FlavorRef: "flavor-uuid",
				},
			},
		},
	}
}

func provisionerOpts(maxSize int) []fleeting.Option {
	return []fleeting.Option{
		fleeting.WithMaxSize(maxSize),
		fleeting.WithUpdateInterval(100 * time.Millisecond),
		fleeting.WithUpdateIntervalWhenExpecting(50 * time.Millisecond),
		fleeting.WithDeletionRetryInterval(100 * time.Millisecond),
		fleeting.WithInstanceGroupSettings(provider.Settings{
			ConnectorConfig: provider.ConnectorConfig{
				Username:             "cloud-user",
				UseStaticCredentials: true,
				Key:                  []byte("not-a-real-key"),
			},
		}),
	}
}

// TestProvisionerLoop_ScaleUpConnectScaleDown drives the whole loop the runner
// runs in production — fleeting's own reconcile, prune, removal, provision and
// Capacity — against the plugin's real Update/Increase/Decrease/ConnectInfo,
// rather than calling those methods directly the way provider_test.go does.
func TestProvisionerLoop_ScaleUpConnectScaleDown(t *testing.T) {
	c := newTenant()

	p, err := fleeting.Init(t.Context(), hclog.NewNullLogger(), newVerifyGroup(c), provisionerOpts(3)...)
	require.NoError(t, err)

	defer p.Shutdown(t.Context())

	p.Request(2)

	require.Eventually(t, func() bool { return p.Capacity().Running == 2 }, 20*time.Second, 50*time.Millisecond,
		"two instances must reach Running through the real provisioner")

	// ConnectInfo, as the runner's connector calls it.
	instances := p.Instances()
	require.Len(t, instances, 2)

	for _, inst := range instances {
		info, err := inst.ConnectInfo(t.Context())
		require.NoError(t, err)

		assert.Equal(t, "cloud-user", info.Username)
		assert.Equal(t, provider.ProtocolSSH, info.Protocol)
		assert.Equal(t, "linux", info.OS)
		assert.Equal(t, "amd64", info.Arch)
		assert.True(t, strings.HasPrefix(info.InternalAddr, "10.20.0."),
			"internal must be the fixed address, got %q", info.InternalAddr)
		assert.True(t, strings.HasPrefix(info.ExternalAddr, "203.0.113."),
			"external must be the floating address, got %q", info.ExternalAddr)
	}

	// Scale to zero the way the executor does.
	for _, inst := range instances {
		inst.Delete()
	}

	require.Eventually(t, func() bool { return len(p.Instances()) == 0 }, 20*time.Second, 50*time.Millisecond,
		"all instances must be reaped")

	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Empty(t, c.servers, "the tenant must be left with no servers")
}

// TestProvisionerLoop_UndeletableInstanceDoesNotHaltProvisioning pins the
// reason Update swallows per-instance failures: a server Nova refuses to
// delete must not stop the group from scaling up. When Update returned that
// error, reconcile bailed and the loop skipped provision() for good.
func TestProvisionerLoop_UndeletableInstanceDoesNotHaltProvisioning(t *testing.T) {
	c := newTenant()
	c.undeletable = "wedged"
	c.deleteErr = errors.New("policy does not allow server delete")
	c.inject("wedged", "ERROR", map[string]string{MetadataKey: "prod-verify"})

	p, err := fleeting.Init(t.Context(), hclog.NewNullLogger(), newVerifyGroup(c), provisionerOpts(3)...)
	require.NoError(t, err, "Init must survive a group that already contains a wedged instance")

	defer p.Shutdown(t.Context())

	p.Request(1)

	require.Eventually(t, func() bool { return p.Capacity().Running == 1 }, 20*time.Second, 50*time.Millisecond,
		"a new instance must still reach Running while the wedged one persists")

	c.mu.Lock()
	defer c.mu.Unlock()
	_, stillThere := c.servers["wedged"]
	assert.True(t, stillThere, "the wedged instance is still in the tenant, as expected")
}

// TestProvisionerLoop_BuiltBinarySpeaksThePluginProtocol checks the shipped
// binary still speaks the go-plugin gRPC protocol the docker-autoscaler
// executor uses, guarding the plugin.Serve -> plugin.Main entrypoint change
// and version.go's now-empty defaults. Unlike TestProvisioning it needs no
// cloud: Init is expected to fail, and what matters is that the failure
// arrives as an application error over a working transport.
func TestProvisionerLoop_BuiltBinarySpeaksThePluginProtocol(t *testing.T) {
	bin := integration.BuildPluginBinary(t, "./cmd/fleeting-plugin-openstack", "fleeting-plugin-openstack")

	runner, err := fleeting.RunPlugin(bin, []byte(`{"name":"prod-verify","cloud":"definitely-not-a-cloud"}`))
	require.NoError(t, err, "handshake with the real binary must succeed")

	defer runner.Kill()

	require.NotNil(t, runner.InstanceGroup())

	// Init must fail cleanly on the missing cloud: a config error travelling
	// back over gRPC, not a transport or handshake failure.
	_, err = runner.InstanceGroup().Init(t.Context(), hclog.NewNullLogger(), provider.Settings{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "connection refused")
	assert.NotContains(t, err.Error(), "Unimplemented")
	t.Logf("Init error surfaced over gRPC as expected: %v", err)
}
