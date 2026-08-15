package fpoc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/ezintz/fleeting-plugin-openstack/internal/openstackclient"
)

// fakeClient is a hand-rolled mock of openstackclient.Client for unit
// testing createInstance. Only the methods exercised by the tests have
// meaningful behaviour; the rest satisfy the interface with zero values.
type fakeClient struct {
	nextPortID   atomic.Int64
	nextServerID atomic.Int64

	createPortCalls   []createPortCall
	createServerCalls []createServerCall
	deletePortCalls   []string

	createServerErr error
	getServerErr    error

	// server, when set, is returned by GetServer — lets ConnectInfo tests
	// present an instance that has reached ACTIVE with an address.
	server *servers.Server
}

type createPortCall struct {
	NetworkID, SubnetID, Description string
	SecurityGroups                   []string
}

type createServerCall struct {
	SpecMap map[string]interface{}
}

func newFakeClient() *fakeClient { return &fakeClient{} }

func (f *fakeClient) CreatePort(_ context.Context, networkID, subnetID, description string, securityGroups []string) (*ports.Port, error) {
	id := fmt.Sprintf("port-%d", f.nextPortID.Add(1))
	f.createPortCalls = append(f.createPortCalls, createPortCall{networkID, subnetID, description, securityGroups})
	return &ports.Port{ID: id, NetworkID: networkID, Description: description}, nil
}

func (f *fakeClient) CreateServer(_ context.Context, spec servers.CreateOptsBuilder, _ servers.SchedulerHintOptsBuilder) (*servers.Server, error) {
	if f.createServerErr != nil {
		return nil, f.createServerErr
	}
	sm, err := spec.ToServerCreateMap()
	if err != nil {
		return nil, err
	}
	f.createServerCalls = append(f.createServerCalls, createServerCall{sm})
	return &servers.Server{ID: fmt.Sprintf("server-%d", f.nextServerID.Add(1))}, nil
}

func (f *fakeClient) DeletePort(_ context.Context, portID string) error {
	f.deletePortCalls = append(f.deletePortCalls, portID)
	return nil
}

// Stubs — required to satisfy the interface, not exercised by tests.
func (f *fakeClient) GetImageProperties(_ context.Context, _ string) (*openstackclient.ImageProperties, error) {
	return &openstackclient.ImageProperties{}, nil
}
func (f *fakeClient) GetImageByName(_ context.Context, _ string) (string, *openstackclient.ImageProperties, error) {
	return "", &openstackclient.ImageProperties{}, nil
}
func (f *fakeClient) GetImageByMetadata(_ context.Context, _ string) (string, *openstackclient.ImageProperties, error) {
	return "", &openstackclient.ImageProperties{}, nil
}
func (f *fakeClient) GetFlavorByName(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (f *fakeClient) ShowServerConsoleOutput(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (f *fakeClient) GetServer(_ context.Context, _ string) (*servers.Server, error) {
	if f.getServerErr != nil {
		return nil, f.getServerErr
	}
	if f.server != nil {
		return f.server, nil
	}
	return &servers.Server{}, nil
}
func (f *fakeClient) ListServers(_ context.Context) ([]servers.Server, error) { return nil, nil }
func (f *fakeClient) DeleteServer(_ context.Context, _ string) error          { return nil }
func (f *fakeClient) ListPortsByDeviceID(_ context.Context, _ string) ([]ports.Port, error) {
	return nil, nil
}

// newTestGroup builds an InstanceGroup wired to fc, with reasonable
// defaults for fields createInstance touches.
func newTestGroup(fc *fakeClient, networks []PluginNetwork) *InstanceGroup {
	return &InstanceGroup{
		Name: "test-asg",
		ServerSpec: ExtCreateOpts{
			CreateOpts: servers.CreateOpts{
				Name:      "vm-%d",
				ImageRef:  "image-uuid",
				FlavorRef: "flavor-uuid",
			},
			Networks: networks,
		},
		client: fc,
		log:    hclog.NewNullLogger(),
	}
}

// serverNameFromCall pulls the "name" the plugin sent to Nova out of the
// captured create payload.
func serverNameFromCall(t *testing.T, call createServerCall) string {
	t.Helper()
	srv, ok := call.SpecMap["server"].(map[string]interface{})
	require.True(t, ok, "create payload has no server map")
	name, ok := srv["name"].(string)
	require.True(t, ok, "create payload has no server name")
	return name
}

// portRefsFromCall extracts the per-network "port" values from the
// CreateServer payload captured by the test fake. ToServerCreateMap
// returns a heterogeneous map[string]any (the networks slice keeps its
// concrete []servers.Network type rather than []interface{}), so we
// round-trip through JSON to get a uniformly-typed structure to walk.
func portRefsFromCall(t *testing.T, call createServerCall) []string {
	t.Helper()
	raw, err := json.Marshal(call.SpecMap)
	require.NoError(t, err)
	var parsed struct {
		Server struct {
			Networks []map[string]string `json:"networks"`
		} `json:"server"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	out := make([]string, 0, len(parsed.Server.Networks))
	for _, n := range parsed.Server.Networks {
		if p, ok := n["port"]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Regression test for the shared-slice bug: createInstance used to mutate
// g.ServerSpec.Networks in place, so the second call resubmitted the
// previous call's port and got 409 / 400 from Nova. This test fails on
// the pre-fix code and passes once spec.Networks is rebuilt fresh.
func TestCreateInstance_SubnetIDFreshPortPerCall(t *testing.T) {
	assert := assert.New(t)

	fc := newFakeClient()
	g := newTestGroup(fc, []PluginNetwork{
		{UUID: "network-uuid", SubnetID: "subnet-uuid"},
	})

	id1, err := g.createInstance(t.Context())
	assert.NoError(err)
	assert.Equal("server-1", id1)
	require.Len(t, fc.createPortCalls, 1, "first call should pre-create one port")
	assert.Equal("subnet-uuid", fc.createPortCalls[0].SubnetID)

	id2, err := g.createInstance(t.Context())
	assert.NoError(err)
	assert.Equal("server-2", id2)
	require.Len(t, fc.createPortCalls, 2, "second call should also pre-create a fresh port — if this is 1, the source spec was mutated and SubnetID got dropped")
	assert.Equal("subnet-uuid", fc.createPortCalls[1].SubnetID)

	assert.Equal("subnet-uuid", g.ServerSpec.Networks[0].SubnetID, "g.ServerSpec.Networks must not be mutated")
	assert.Empty(g.ServerSpec.Networks[0].Port, "g.ServerSpec.Networks must not be mutated")

	require.Len(t, fc.createServerCalls, 2)
	ports1 := portRefsFromCall(t, fc.createServerCalls[0])
	ports2 := portRefsFromCall(t, fc.createServerCalls[1])
	require.Len(t, ports1, 1)
	require.Len(t, ports2, 1)
	assert.Equal("port-1", ports1[0])
	assert.Equal("port-2", ports2[0])
	assert.NotEqual(ports1[0], ports2[0], "each createInstance call must attach a distinct pre-created port")
}

func TestCreateInstance_NoSubnetIDSkipsPortPreCreation(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, []PluginNetwork{
		{UUID: "network-uuid"},
	})

	id, err := g.createInstance(t.Context())
	assert.NoError(t, err)
	assert.Equal(t, "server-1", id)
	assert.Empty(t, fc.createPortCalls, "no SubnetID -> no port pre-creation")
	assert.Empty(t, fc.deletePortCalls, "nothing to clean up")
}

func TestCreateInstance_MixedNetworksOnlyPreCreatesForSubnetEntries(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, []PluginNetwork{
		{UUID: "net-a"},                       // Nova-managed, no port pre-creation
		{UUID: "net-b", SubnetID: "subnet-b"}, // pinned via pre-created port
		{UUID: "net-c", SubnetID: "subnet-c"}, // pinned via pre-created port
	})

	_, err := g.createInstance(t.Context())
	assert.NoError(t, err)
	require.Len(t, fc.createPortCalls, 2)
	assert.Equal(t, "subnet-b", fc.createPortCalls[0].SubnetID)
	assert.Equal(t, "subnet-c", fc.createPortCalls[1].SubnetID)
}

func TestCreateInstance_SubnetIDPortInheritsSecurityGroups(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, []PluginNetwork{
		{UUID: "n", SubnetID: "s"},
	})
	g.ServerSpec.SecurityGroups = []string{"sg-uuid-1", "sg-uuid-2"}

	_, err := g.createInstance(t.Context())
	assert.NoError(t, err)
	require.Len(t, fc.createPortCalls, 1)
	assert.Equal(t, []string{"sg-uuid-1", "sg-uuid-2"}, fc.createPortCalls[0].SecurityGroups,
		"pre-created port must inherit server_spec.security_groups, otherwise the worker boots with only the tenant default group")
}

func TestCreateInstance_SubnetIDPortNoSecurityGroupsWhenUnset(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, []PluginNetwork{
		{UUID: "n", SubnetID: "s"},
	})
	// g.ServerSpec.SecurityGroups intentionally left nil — Neutron will
	// fall back to the tenant default and we should not over-specify.

	_, err := g.createInstance(t.Context())
	assert.NoError(t, err)
	require.Len(t, fc.createPortCalls, 1)
	assert.Nil(t, fc.createPortCalls[0].SecurityGroups,
		"when no security_groups are configured the plugin must not pass an empty list (which would mean 'attach no SGs at all')")
}

// newConnectInfoGroup builds a group whose single instance is ACTIVE and
// addressable, ready for ConnectInfo to be called against it.
func newConnectInfoGroup(t *testing.T) *InstanceGroup {
	t.Helper()

	fc := newFakeClient()
	fc.server = &servers.Server{ID: "server-1", Status: "ACTIVE", AccessIPv4: "10.0.0.5"}

	g := newTestGroup(fc, nil)
	g.UseIgnition = true

	return g
}

// createInstance authorizes the plugin's SSH key for os_admin_user when no
// username is configured, but ConnectInfo used to report g.settings.Username
// verbatim — i.e. "" — so the connector dialed with an empty SSH user and
// authentication failed. fleeting's own acceptance suite guards this with
// require.NotEmpty(t, info.Username).
func TestConnectInfo_UsernameFallsBackToOSAdminUser(t *testing.T) {
	g := newConnectInfoGroup(t)
	g.settings = provider.Settings{} // connector_config.username deliberately unset
	g.imgProps.Store(&openstackclient.ImageProperties{OSAdminUser: "core"})

	info, err := g.ConnectInfo(t.Context(), "server-1")
	require.NoError(t, err)

	assert.Equal(t, "core", info.Username)
	assert.Equal(t, "10.0.0.5", info.ExternalAddr)
}

// An explicitly configured username must win over the image property.
func TestConnectInfo_ConfiguredUsernameWinsOverOSAdminUser(t *testing.T) {
	g := newConnectInfoGroup(t)
	g.settings = provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{Username: "operator"},
	}
	g.imgProps.Store(&openstackclient.ImageProperties{OSAdminUser: "core"})

	info, err := g.ConnectInfo(t.Context(), "server-1")
	require.NoError(t, err)

	assert.Equal(t, "operator", info.Username)
}

// With neither source available, fail loudly rather than handing the
// connector an empty user that dies with an opaque SSH error.
func TestConnectInfo_ErrorsWhenNoUsernameAvailable(t *testing.T) {
	g := newConnectInfoGroup(t)
	g.settings = provider.Settings{}
	g.imgProps.Store(&openstackclient.ImageProperties{})

	_, err := g.ConnectInfo(t.Context(), "server-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "os_admin_user")
}

// The same fallback must drive the Ignition key injection, so the account
// the key is authorized for matches the one ConnectInfo reports.
func TestCreateInstance_IgnitionUsesOSAdminUserForKeyInjection(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, nil)
	g.UseIgnition = true
	g.sshPubKey = "ssh-rsa AAAAB3NzaC1yc2E test"
	g.settings = provider.Settings{}
	g.imgProps.Store(&openstackclient.ImageProperties{OSAdminUser: "core"})

	_, err := g.createInstance(t.Context())
	require.NoError(t, err)

	require.Len(t, fc.createServerCalls, 1)

	// gophercloud base64-encodes user_data into the create payload, so
	// decode it back to the Ignition config before inspecting it.
	raw, err := json.Marshal(fc.createServerCalls[0].SpecMap)
	require.NoError(t, err)
	var parsed struct {
		Server struct {
			UserData string `json:"user_data"`
		} `json:"server"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.NotEmpty(t, parsed.Server.UserData)

	ign, err := base64.StdEncoding.DecodeString(parsed.Server.UserData)
	require.NoError(t, err)

	var cfg struct {
		Passwd struct {
			Users []struct {
				Name              string   `json:"name"`
				SSHAuthorizedKeys []string `json:"sshAuthorizedKeys"`
			} `json:"users"`
		} `json:"passwd"`
	}
	require.NoError(t, json.Unmarshal(ign, &cfg))
	require.Len(t, cfg.Passwd.Users, 1)
	assert.Equal(t, "core", cfg.Passwd.Users[0].Name,
		"the key must be authorized for the same account ConnectInfo reports")
	assert.Equal(t, []string{g.sshPubKey}, cfg.Passwd.Users[0].SSHAuthorizedKeys)
}

func TestCreateInstance_CleanupPortsOnServerFailure(t *testing.T) {
	fc := newFakeClient()
	fc.createServerErr = errors.New("nova exploded")

	g := newTestGroup(fc, []PluginNetwork{
		{UUID: "n", SubnetID: "s"},
	})

	id, err := g.createInstance(t.Context())
	assert.Error(t, err)
	assert.Empty(t, id)
	require.Len(t, fc.createPortCalls, 1, "port should still have been pre-created")
	assert.Equal(t, []string{"port-1"}, fc.deletePortCalls, "pre-created port must be cleaned up when CreateServer fails")
}

func TestValidateNameTemplate(t *testing.T) {
	tests := []struct {
		name     string
		template string
		wantErr  bool
	}{
		{"decimal verb", "vm-%d", false},
		{"zero padded verb", "runner-%03d", false},
		{"verb in the middle", "ci-%d-worker", false},
		{"no verb", "runner", true},
		{"escaped percent only", "runner-100%%", true},
		{"string verb", "runner-%s", true},
		{"two verbs", "runner-%d-%d", true},
		{"unknown verb", "runner-%y", true},
		{"empty", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNameTemplate(tc.template)
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "server_spec.name")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// Without validation, a template lacking an integer verb reaches Nova with
// fmt's error marker baked into the server name. Pin that so the Init guard
// cannot regress unnoticed.
func TestCreateInstance_NameWithoutVerbLeaksFmtMarker(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, nil)
	g.ServerSpec.Name = "runner"

	require.Error(t, validateNameTemplate(g.ServerSpec.Name))

	_, err := g.createInstance(t.Context())
	require.NoError(t, err)

	require.Len(t, fc.createServerCalls, 1)
	require.Equal(t, "runner%!(EXTRA int=1)", serverNameFromCall(t, fc.createServerCalls[0]))
}

// A valid template is expanded to a distinct name per instance.
func TestCreateInstance_NameTemplateExpandsPerInstance(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, nil)
	g.ServerSpec.Name = "runner-%d"

	require.NoError(t, validateNameTemplate(g.ServerSpec.Name))

	for range 2 {
		_, err := g.createInstance(t.Context())
		require.NoError(t, err)
	}

	require.Len(t, fc.createServerCalls, 2)
	require.Equal(t, "runner-1", serverNameFromCall(t, fc.createServerCalls[0]))
	require.Equal(t, "runner-2", serverNameFromCall(t, fc.createServerCalls[1]))
}

func TestHeartbeat_ActiveInstanceIsHealthy(t *testing.T) {
	fc := newFakeClient()
	fc.server = &servers.Server{ID: "server-1", Status: "ACTIVE"}
	g := newTestGroup(fc, nil)

	require.NoError(t, g.Heartbeat(t.Context(), "server-1"))
}

func TestHeartbeat_NonActiveInstanceIsUnhealthy(t *testing.T) {
	fc := newFakeClient()
	fc.server = &servers.Server{ID: "server-1", Status: "SHUTOFF"}
	g := newTestGroup(fc, nil)

	err := g.Heartbeat(t.Context(), "server-1")
	require.Error(t, err)
	require.ErrorIs(t, err, provider.ErrInstanceUnhealthy)
	require.Contains(t, err.Error(), "SHUTOFF")
}

func TestHeartbeat_GetServerErrorPropagates(t *testing.T) {
	fc := newFakeClient()
	fc.getServerErr = errors.New("boom")
	g := newTestGroup(fc, nil)

	err := g.Heartbeat(t.Context(), "server-1")
	require.Error(t, err)
	require.ErrorIs(t, err, fc.getServerErr)
}

// Suspend/Resume must satisfy provider.InstanceGroup, but Init never
// advertises provider.CapabilitySuspendResume, so the runner's provisioner
// never actually calls them. Pin that they report "not supported" rather
// than silently pretending to succeed.
func TestSuspendResume_ReportNotSupported(t *testing.T) {
	g := newTestGroup(newFakeClient(), nil)

	succeeded, err := g.Suspend(t.Context(), []string{"server-1"})
	require.Nil(t, succeeded)
	require.ErrorIs(t, err, provider.ErrSuspendResumeNotSupported)

	succeeded, err = g.Resume(t.Context(), []string{"server-1"})
	require.Nil(t, succeeded)
	require.ErrorIs(t, err, provider.ErrSuspendResumeNotSupported)
}
