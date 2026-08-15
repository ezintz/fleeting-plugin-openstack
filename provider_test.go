package fpoc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
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
//
// Decrease deletes instances concurrently and cleanupPorts deletes an
// instance's ports concurrently, so every method that records a call or
// touches the shared maps holds mu.
type fakeClient struct {
	nextPortID   atomic.Int64
	nextServerID atomic.Int64

	mu                sync.Mutex
	createPortCalls   []createPortCall
	createServerCalls []createServerCall
	deletePortCalls   []string
	deleteServerCalls []string

	createServerErr error
	getServerErr    error

	// deleteServerFunc, when set, overrides DeleteServer's return value —
	// lets tests simulate DeleteServer failing (e.g. for the Update
	// self-heal path) or a 404 (already gone).
	deleteServerFunc func(serverID string) error

	// deletePortFunc, when set, overrides DeletePort's return value — lets
	// tests simulate Neutron's 409 conflict / 404 not-found responses
	// during the Nova server-teardown race.
	deletePortFunc func(portID string) error

	// createdPortsByID and portsByDevice track the port<->server
	// association CreatePort/CreateServer establish, so ListPortsByDeviceID
	// can answer Decrease's pre-deletion port lookup like the real API does.
	createdPortsByID map[string]ports.Port
	portsByDevice    map[string][]ports.Port

	// server, when set, is returned by GetServer — lets ConnectInfo tests
	// present an instance that has reached ACTIVE with an address.
	server *servers.Server

	// listServersResult, when set, is returned by ListServers — lets Update
	// tests present a group's current instances.
	listServersResult []servers.Server

	// listServersErr and consoleOutputErr, when set, make ListServers /
	// ShowServerConsoleOutput fail — the two API failures Update has to
	// treat differently (fatal to the cycle vs. per-instance).
	listServersErr   error
	consoleOutputErr error
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
	f.mu.Lock()
	defer f.mu.Unlock()

	id := fmt.Sprintf("port-%d", f.nextPortID.Add(1))
	f.createPortCalls = append(f.createPortCalls, createPortCall{networkID, subnetID, description, securityGroups})
	p := ports.Port{ID: id, NetworkID: networkID, Description: description}
	if f.createdPortsByID == nil {
		f.createdPortsByID = make(map[string]ports.Port)
	}
	f.createdPortsByID[id] = p
	return &p, nil
}

func (f *fakeClient) CreateServer(_ context.Context, spec servers.CreateOptsBuilder, _ servers.SchedulerHintOptsBuilder) (*servers.Server, error) {
	if f.createServerErr != nil {
		return nil, f.createServerErr
	}
	sm, err := spec.ToServerCreateMap()
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.createServerCalls = append(f.createServerCalls, createServerCall{sm})

	id := fmt.Sprintf("server-%d", f.nextServerID.Add(1))
	for _, portID := range extractPortRefs(sm) {
		if p, ok := f.createdPortsByID[portID]; ok {
			if f.portsByDevice == nil {
				f.portsByDevice = make(map[string][]ports.Port)
			}
			f.portsByDevice[id] = append(f.portsByDevice[id], p)
		}
	}

	return &servers.Server{ID: id}, nil
}

func (f *fakeClient) DeletePort(_ context.Context, portID string) error {
	f.mu.Lock()
	f.deletePortCalls = append(f.deletePortCalls, portID)
	deletePortFunc := f.deletePortFunc
	f.mu.Unlock()

	if deletePortFunc != nil {
		return deletePortFunc(portID)
	}
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
	if f.consoleOutputErr != nil {
		return "", f.consoleOutputErr
	}
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
func (f *fakeClient) ListServers(_ context.Context) ([]servers.Server, error) {
	if f.listServersErr != nil {
		return nil, f.listServersErr
	}
	return f.listServersResult, nil
}

func (f *fakeClient) DeleteServer(_ context.Context, serverID string) error {
	f.mu.Lock()
	f.deleteServerCalls = append(f.deleteServerCalls, serverID)
	deleteServerFunc := f.deleteServerFunc
	f.mu.Unlock()

	if deleteServerFunc != nil {
		return deleteServerFunc(serverID)
	}
	return nil
}

func (f *fakeClient) ListPortsByDeviceID(_ context.Context, deviceID string) ([]ports.Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.portsByDevice[deviceID], nil
}

// deletedServers returns the recorded DeleteServer calls, sorted so tests
// can assert on them regardless of the order concurrent deletes finished in.
func (f *fakeClient) deletedServers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := slices.Clone(f.deleteServerCalls)
	slices.Sort(out)

	return out
}

// deletedPorts returns the recorded DeletePort calls, sorted; see
// deletedServers.
func (f *fakeClient) deletedPorts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := slices.Clone(f.deletePortCalls)
	slices.Sort(out)

	return out
}

// extractPortRefs pulls the per-network "port" values out of a CreateServer
// payload. ToServerCreateMap returns a heterogeneous map[string]any (the
// networks slice keeps its concrete []servers.Network type rather than
// []interface{}), so we round-trip through JSON to get a uniformly-typed
// structure to walk.
func extractPortRefs(specMap map[string]interface{}) []string {
	raw, err := json.Marshal(specMap)
	if err != nil {
		return nil
	}
	var parsed struct {
		Server struct {
			Networks []map[string]string `json:"networks"`
		} `json:"server"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	out := make([]string, 0, len(parsed.Server.Networks))
	for _, n := range parsed.Server.Networks {
		if p, ok := n["port"]; ok {
			out = append(out, p)
		}
	}
	return out
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
	return extractPortRefs(call.SpecMap)
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

// respondWithStatus builds the gophercloud error DeletePort returns for a
// given HTTP status, matching what gophercloud.ResponseCodeIs checks for.
func respondWithStatus(status int) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: status}
}

// DELETE /servers/{id} answers before Nova finishes detaching the server's
// ports in the background, so an immediate DeletePort on a pre-created port
// can race that teardown and see a 409 while the port is still owned by the
// still-deleting server. This test simulates exactly one such conflict and
// asserts deletePortRetryingConflict retries rather than giving up — it
// takes ~1s of real wall-clock time because gophercloud.WaitFor polls on a
// fixed 1-second ticker.
func TestDeletePortRetryingConflict_RetriesOnConflictThenSucceeds(t *testing.T) {
	fc := newFakeClient()
	attempts := 0
	fc.deletePortFunc = func(string) error {
		attempts++
		if attempts == 1 {
			return respondWithStatus(http.StatusConflict)
		}
		return nil
	}
	g := newTestGroup(fc, nil)

	err := g.deletePortRetryingConflict(t.Context(), "port-1")
	require.NoError(t, err)
	assert.Equal(t, 2, attempts)
}

// A 404 means the port is already gone — Nova's own teardown or a previous
// attempt beat us to it — and must be treated as success, not an error.
func TestDeletePortRetryingConflict_NotFoundIsSuccess(t *testing.T) {
	fc := newFakeClient()
	fc.deletePortFunc = func(string) error {
		return respondWithStatus(http.StatusNotFound)
	}
	g := newTestGroup(fc, nil)

	require.NoError(t, g.deletePortRetryingConflict(t.Context(), "port-1"))
	assert.Len(t, fc.deletePortCalls, 1, "404 must not be retried")
}

// A port that is never released (e.g. the server delete itself failed
// server-side) must not retry forever — deletePortRetryingConflict gives up
// once its context deadline passes and returns that error to the caller.
func TestDeletePortRetryingConflict_GivesUpWhenContextExpires(t *testing.T) {
	fc := newFakeClient()
	fc.deletePortFunc = func(string) error {
		return respondWithStatus(http.StatusConflict)
	}
	g := newTestGroup(fc, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := g.deletePortRetryingConflict(ctx, "port-1")
	require.Error(t, err)
	assert.GreaterOrEqual(t, len(fc.deletePortCalls), 1)
}

// Errors other than 409/404 (e.g. auth failures, 500s) are real failures,
// not the teardown race, and must not be retried.
func TestDeletePortRetryingConflict_OtherErrorsAreNotRetried(t *testing.T) {
	fc := newFakeClient()
	wantErr := errors.New("boom")
	fc.deletePortFunc = func(string) error {
		return wantErr
	}
	g := newTestGroup(fc, nil)

	err := g.deletePortRetryingConflict(t.Context(), "port-1")
	require.ErrorIs(t, err, wantErr)
	assert.Len(t, fc.deletePortCalls, 1)
}

// End-to-end through Decrease: a port that loses the teardown race once
// still gets cleaned up, and the instance is still reported as
// successfully deleted (port cleanup failures never fail the deletion —
// the server really is gone either way).
func TestDecrease_PortCleanupRetriesConflictThenSucceeds(t *testing.T) {
	fc := newFakeClient()
	attempts := 0
	fc.deletePortFunc = func(string) error {
		attempts++
		if attempts == 1 {
			return respondWithStatus(http.StatusConflict)
		}
		return nil
	}
	g := newTestGroup(fc, []PluginNetwork{{UUID: "network-uuid", SubnetID: "subnet-uuid"}})

	id, err := g.createInstance(t.Context())
	require.NoError(t, err)

	succeeded, err := g.Decrease(t.Context(), []string{id})
	require.NoError(t, err)
	assert.Equal(t, []string{id}, succeeded)
	assert.Equal(t, 2, attempts)
}

// updateCall records one (id, state) pair reported through Update's
// callback.
type updateCall struct {
	ID    string
	State provider.State
}

func collectUpdates(g *InstanceGroup, t *testing.T) ([]updateCall, error) {
	t.Helper()
	var calls []updateCall
	err := g.Update(t.Context(), func(id string, state provider.State) {
		calls = append(calls, updateCall{id, state})
	})
	return calls, err
}

// A Nova instance that reaches ERROR will never become usable again, and
// GitLab Runner's provisioner only calls Decrease for instances an operator
// explicitly requested removal of — it never acts on its own initiative
// just because Update reports a bad state. Without deleting it itself, the
// plugin would leak the instance forever. This is the fix for reporting it
// as StateTimeout, which had the exact same leak (see deleteInstance).
func TestUpdate_ErrorInstanceIsSelfDeleted(t *testing.T) {
	fc := newFakeClient()
	fc.listServersResult = []servers.Server{
		{ID: "server-1", Status: "ERROR", Metadata: map[string]string{MetadataKey: "test-asg"}},
	}
	g := newTestGroup(fc, nil)

	calls, err := collectUpdates(g, t)
	require.NoError(t, err)

	require.Equal(t, []string{"server-1"}, fc.deleteServerCalls, "ERROR instance must be deleted by the plugin itself")
	require.Equal(t, []updateCall{{"server-1", provider.StateDeleting}}, calls)
}

// SHUTOFF and UNKNOWN instances are equally abandoned by Nova and equally
// invisible to the runner's own removal logic, so they must be self-deleted
// too, not just relabeled.
func TestUpdate_ShutoffAndUnknownInstancesAreSelfDeleted(t *testing.T) {
	for _, status := range []string{"SHUTOFF", "UNKNOWN"} {
		t.Run(status, func(t *testing.T) {
			fc := newFakeClient()
			fc.listServersResult = []servers.Server{
				{ID: "server-1", Status: status, Metadata: map[string]string{MetadataKey: "test-asg"}},
			}
			g := newTestGroup(fc, nil)

			calls, err := collectUpdates(g, t)
			require.NoError(t, err)

			require.Equal(t, []string{"server-1"}, fc.deleteServerCalls)
			require.Equal(t, []updateCall{{"server-1", provider.StateDeleting}}, calls)
		})
	}
}

// A pre-created port on a broken instance must be cleaned up the same way
// Decrease would clean it up, including the async-teardown retry.
func TestUpdate_ErrorInstanceCleansUpItsPorts(t *testing.T) {
	fc := newFakeClient()
	fc.portsByDevice = map[string][]ports.Port{
		"server-1": {{ID: "port-1", Description: PortDescription}},
	}
	fc.listServersResult = []servers.Server{
		{ID: "server-1", Status: "ERROR", Metadata: map[string]string{MetadataKey: "test-asg"}},
	}
	g := newTestGroup(fc, nil)

	_, err := collectUpdates(g, t)
	require.NoError(t, err)
	require.Equal(t, []string{"port-1"}, fc.deletePortCalls)
}

// A DeleteServer failure must NOT fail the update. fleeting's provisioner
// treats a non-nil Update as "this snapshot is untrustworthy" and skips
// provision() for the whole cycle; since the plugin reports these instances
// as StateDeleting and removal() skips StateDeleting instances, an instance
// Nova refuses to delete (a locked server, a policy denial) would otherwise
// stop the group from ever scaling up again. Report StateDeleting so the
// instance stays tracked and the next cycle retries the delete.
func TestUpdate_ErrorInstanceDeleteFailureDoesNotFailTheUpdate(t *testing.T) {
	fc := newFakeClient()
	fc.deleteServerFunc = func(string) error { return errors.New("nova unavailable") }
	fc.listServersResult = []servers.Server{
		{ID: "server-1", Status: "ERROR", Metadata: map[string]string{MetadataKey: "test-asg"}},
	}
	g := newTestGroup(fc, nil)

	calls, err := collectUpdates(g, t)
	require.NoError(t, err, "a per-instance delete failure must not fail the whole update cycle")
	require.Equal(t, []updateCall{{"server-1", provider.StateDeleting}}, calls)

	// Second cycle: the instance is still listed, so the delete is retried.
	calls, err = collectUpdates(g, t)
	require.NoError(t, err)
	require.Equal(t, []updateCall{{"server-1", provider.StateDeleting}}, calls)
	require.Equal(t, []string{"server-1", "server-1"}, fc.deletedServers(), "the failed delete must be retried next cycle")
}

// A healthy instance in the same batch must still be reported normally when
// a sibling's delete fails — the failure is contained to its own instance.
func TestUpdate_DeleteFailureDoesNotSuppressHealthyInstances(t *testing.T) {
	fc := newFakeClient()
	fc.deleteServerFunc = func(string) error { return errors.New("nova unavailable") }
	fc.listServersResult = []servers.Server{
		{ID: "broken", Status: "ERROR", Metadata: map[string]string{MetadataKey: "test-asg"}},
		{ID: "healthy", Status: "ACTIVE", Created: time.Now().Add(-time.Hour), Metadata: map[string]string{MetadataKey: "test-asg"}},
	}
	g := newTestGroup(fc, nil)

	calls, err := collectUpdates(g, t)
	require.NoError(t, err)
	require.Equal(t, []updateCall{
		{"broken", provider.StateDeleting},
		{"healthy", provider.StateRunning},
	}, calls)
}

// Console output isn't always available the moment a server reports ACTIVE.
// That must leave the instance reported as still creating, not drop it from
// the update entirely: an unreported instance accumulates missed updates and
// the provisioner prunes it as timed out after five of them, while the real
// server keeps running and billing.
func TestUpdate_ConsoleOutputFailureStillReportsInstance(t *testing.T) {
	fc := newFakeClient()
	fc.consoleOutputErr = errors.New("console not available yet")
	fc.listServersResult = []servers.Server{
		{ID: "server-1", Status: "ACTIVE", Created: time.Now(), Metadata: map[string]string{MetadataKey: "test-asg"}},
	}
	g := newTestGroup(fc, nil)
	g.BootTime = time.Hour // force the console-output path

	calls, err := collectUpdates(g, t)
	require.NoError(t, err, "an unreadable console log means 'not ready yet', not a failed cycle")
	require.Equal(t, []updateCall{{"server-1", provider.StateCreating}}, calls)
}

// Failing to enumerate the group at all IS fatal to the cycle: the snapshot
// really is untrustworthy, and acting on it could double-provision.
func TestUpdate_ListServersFailureIsReturned(t *testing.T) {
	fc := newFakeClient()
	wantErr := errors.New("nova unreachable")
	fc.listServersErr = wantErr
	g := newTestGroup(fc, nil)

	_, err := collectUpdates(g, t)
	require.ErrorIs(t, err, wantErr)
}

// runners.autoscaler.connector_config lets an operator set protocol/os/arch
// explicitly. ConnectInfo must treat those as an override, not a default
// that image-property detection is free to clobber — matching the
// precedence resolveUsername already applies to Username.
func TestConnectInfo_OperatorProtocolOverridesImageDetection(t *testing.T) {
	g := newConnectInfoGroup(t)
	g.settings.Protocol = provider.ProtocolWinRM
	g.imgProps.Store(&openstackclient.ImageProperties{OSType: "linux", OSAdminUser: "core"})

	info, err := g.ConnectInfo(t.Context(), "server-1")
	require.NoError(t, err)
	assert.Equal(t, provider.ProtocolWinRM, info.Protocol, "operator-configured protocol must not be overwritten by linux image detection")
}

func TestConnectInfo_OperatorOSAndArchOverrideImageDetection(t *testing.T) {
	g := newConnectInfoGroup(t)
	g.settings.OS = "freebsd"
	g.settings.Arch = "arm64"
	g.imgProps.Store(&openstackclient.ImageProperties{OSType: "linux", Architecture: "x86_64", OSAdminUser: "core"})

	info, err := g.ConnectInfo(t.Context(), "server-1")
	require.NoError(t, err)
	assert.Equal(t, "freebsd", info.OS)
	assert.Equal(t, "arm64", info.Arch)
}

// Without an operator override, image properties still drive OS/Arch/
// Protocol detection exactly as before.
func TestConnectInfo_ImageDetectionFillsUnsetFields(t *testing.T) {
	g := newConnectInfoGroup(t)
	g.imgProps.Store(&openstackclient.ImageProperties{OSType: "windows", Architecture: "aarch64", OSAdminUser: "Administrator"})

	info, err := g.ConnectInfo(t.Context(), "server-1")
	require.NoError(t, err)
	assert.Equal(t, provider.ProtocolWinRM, info.Protocol)
	assert.Equal(t, "windows", info.OS)
	assert.Equal(t, "arm64", info.Arch)
}

// No image properties at all (e.g. image_name/image_ref_from_metadata
// unset) still defaults to linux/amd64/ssh.
func TestConnectInfo_NoImagePropertiesDefaultsToLinuxAmd64SSH(t *testing.T) {
	g := newConnectInfoGroup(t)
	g.settings.Username = "core"

	info, err := g.ConnectInfo(t.Context(), "server-1")
	require.NoError(t, err)
	assert.Equal(t, provider.ProtocolSSH, info.Protocol)
	assert.Equal(t, "linux", info.OS)
	assert.Equal(t, "amd64", info.Arch)
}

// copier.Copy is shallow for maps and slices, so every field createInstance
// mutates has to be cloned first. Metadata is the subtle one: without the
// clone, tagging the instance with MetadataKey writes straight through into
// g.ServerSpec.Metadata, leaving the operator's configured spec permanently
// modified — and mutated concurrently if Increase ever runs in parallel.
func TestCreateInstance_DoesNotMutateSourceSpecMetadata(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, nil)
	g.ServerSpec.Metadata = map[string]string{"owner": "platform"}

	_, err := g.createInstance(t.Context())
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"owner": "platform"}, g.ServerSpec.Metadata,
		"the group's configured metadata must not gain the per-instance cluster tag")

	// The tag must still reach Nova on the instance itself.
	require.Len(t, fc.createServerCalls, 1)
	assert.Equal(t, map[string]any{"owner": "platform", MetadataKey: "test-asg"},
		serverFieldFromCall(t, fc.createServerCalls[0], "metadata"))
}

// Same aliasing hazard for the boot-volume block device, and for Metadata
// again. Sequentially the block-device append is invisible (it writes past
// the source slice's length), so the failure mode only shows up when two
// creates overlap: both write the same shared backing array and the same
// shared map. Run under -race, this is what proves the clones are load
// bearing rather than decorative.
func TestCreateInstance_ConcurrentCreatesDoNotShareSpecState(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, nil)
	g.ServerSpec.ImageRef = "image-uuid"
	g.ServerSpec.Metadata = map[string]string{"owner": "platform"}
	// A data volume the operator configured, in a slice with spare
	// capacity — so an un-cloned append writes in place instead of
	// reallocating.
	g.ServerSpec.BlockDevice = append(make([]servers.BlockDevice, 0, 4), servers.BlockDevice{
		BootIndex:       -1,
		SourceType:      servers.SourceBlank,
		DestinationType: servers.DestinationVolume,
		VolumeSize:      10,
	})
	g.VolumeType = "ssd"
	g.VolumeSize = 40

	const concurrency = 8

	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, err := g.createInstance(t.Context())
			assert.NoError(t, err)
		}()
	}

	wg.Wait()

	assert.Equal(t, map[string]string{"owner": "platform"}, g.ServerSpec.Metadata,
		"the group's configured metadata must not gain the per-instance cluster tag")
	assert.Len(t, g.ServerSpec.BlockDevice, 1,
		"the group's configured block devices must not accumulate boot volumes")

	require.Len(t, fc.createServerCalls, concurrency)
	for i, call := range fc.createServerCalls {
		bdm, ok := serverFieldFromCall(t, call, "block_device_mapping_v2").([]any)
		require.True(t, ok, "call %d has no block device mapping", i)
		assert.Len(t, bdm, 2, "call %d must carry the operator's data volume plus exactly one boot volume", i)
	}
}

// Decrease deletes concurrently so a batch can't stall the provisioner's
// single reconcile goroutine for portCleanupTimeout per instance. The
// succeeded list must still come back in the caller's order.
func TestDecrease_DeletesConcurrentlyAndPreservesOrder(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, nil)

	ids := []string{"server-c", "server-a", "server-b"}

	succeeded, err := g.Decrease(t.Context(), ids)
	require.NoError(t, err)
	assert.Equal(t, ids, succeeded, "succeeded must mirror the caller's order, not completion order")
	assert.Equal(t, []string{"server-a", "server-b", "server-c"}, fc.deletedServers())
}

// A batch where one instance fails must still report the rest as deleted,
// and must attribute the error rather than dropping it.
func TestDecrease_PartialFailureReportsTheRest(t *testing.T) {
	fc := newFakeClient()
	wantErr := errors.New("nova unavailable")
	fc.deleteServerFunc = func(serverID string) error {
		if serverID == "server-b" {
			return wantErr
		}

		return nil
	}
	g := newTestGroup(fc, nil)

	succeeded, err := g.Decrease(t.Context(), []string{"server-a", "server-b", "server-c"})
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, []string{"server-a", "server-c"}, succeeded)
}

// portCleanupTimeout is a budget for the whole cleanup, not per port: an
// instance's ports are released by the same background teardown at roughly
// the same moment, so charging each port its own timeout would multiply the
// worst-case stall for no benefit. Ports are therefore deleted concurrently
// and share one deadline.
func TestCleanupPorts_DeletesConcurrentlyUnderOneDeadline(t *testing.T) {
	fc := newFakeClient()
	released := make(chan struct{})
	fc.deletePortFunc = func(string) error {
		// Every port stays conflicted until the teardown "completes",
		// which only happens once all of them are in flight — so this
		// can only finish if the deletes overlap.
		<-released

		return nil
	}
	g := newTestGroup(fc, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		g.cleanupPorts(t.Context(), []string{"port-1", "port-2", "port-3"})
	}()

	require.Eventually(t, func() bool { return len(fc.deletedPorts()) == 3 }, 5*time.Second, 10*time.Millisecond,
		"all three DeletePort calls must be in flight at once")
	close(released)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanupPorts did not return")
	}

	assert.Equal(t, []string{"port-1", "port-2", "port-3"}, fc.deletedPorts())
}

// The connector dials InternalAddr unless use_external_addr is set, so the
// fixed address has to land there and the floating one on ExternalAddr —
// otherwise a runner sharing the tenant network is routed out through NAT.
func TestConnectInfo_ReportsFixedAsInternalAndFloatingAsExternal(t *testing.T) {
	fc := newFakeClient()
	fc.server = &servers.Server{
		ID:     "server-1",
		Status: "ACTIVE",
		Addresses: map[string]any{
			"tenant": []any{
				map[string]any{"addr": "10.0.0.5", "version": float64(4), "OS-EXT-IPS:type": "fixed"},
				map[string]any{"addr": "203.0.113.9", "version": float64(4), "OS-EXT-IPS:type": "floating"},
			},
		},
	}

	g := newTestGroup(fc, nil)
	g.settings.Username = "core"

	info, err := g.ConnectInfo(t.Context(), "server-1")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.5", info.InternalAddr)
	assert.Equal(t, "203.0.113.9", info.ExternalAddr)
}

// An explicitly configured accessIPv4 is the operator naming the address to
// use, so it wins for both endpoints.
func TestConnectInfo_AccessIPv4OverridesAddressSelection(t *testing.T) {
	fc := newFakeClient()
	fc.server = &servers.Server{
		ID:         "server-1",
		Status:     "ACTIVE",
		AccessIPv4: "192.0.2.7",
		Addresses: map[string]any{
			"tenant": []any{
				map[string]any{"addr": "10.0.0.5", "version": float64(4), "OS-EXT-IPS:type": "fixed"},
			},
		},
	}

	g := newTestGroup(fc, nil)
	g.settings.Username = "core"

	info, err := g.ConnectInfo(t.Context(), "server-1")
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.7", info.InternalAddr)
	assert.Equal(t, "192.0.2.7", info.ExternalAddr)
}

// A server Nova no longer knows about is unhealthy, not an API failure —
// the runner has to stop scheduling onto it rather than retrying a lookup.
func TestHeartbeat_MissingServerIsUnhealthy(t *testing.T) {
	fc := newFakeClient()
	fc.getServerErr = gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusNotFound}
	g := newTestGroup(fc, nil)

	err := g.Heartbeat(t.Context(), "server-1")
	require.ErrorIs(t, err, provider.ErrInstanceUnhealthy)
	assert.Contains(t, err.Error(), "no longer exists")
}

// serverFieldFromCall reads one field out of the server object in a captured
// CreateServer payload. ToServerCreateMap returns a heterogeneous
// map[string]any whose nested values keep their concrete Go types, so this
// round-trips through JSON for uniformly-typed values to assert on — the
// same reason extractPortRefs does.
func serverFieldFromCall(t *testing.T, call createServerCall, field string) any {
	t.Helper()

	raw, err := json.Marshal(call.SpecMap)
	require.NoError(t, err)

	var parsed struct {
		Server map[string]any `json:"server"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))

	return parsed.Server[field]
}

// Statuses an instance cannot recover from on its own must be self-deleted,
// the same as ERROR/SHUTOFF. SUSPENDED, SHELVED and SHELVED_OFFLOADED need
// an explicit resume/unshelve the plugin never issues (Init doesn't
// advertise CapabilitySuspendResume) and SOFT_DELETED is awaiting reclaim,
// so without this they sat in StateCreating forever, holding a slot in the
// group while nothing pruned them.
func TestUpdate_UnrecoverableStatusesAreSelfDeleted(t *testing.T) {
	for _, status := range []string{"SOFT_DELETED", "SUSPENDED", "SHELVED", "SHELVED_OFFLOADED"} {
		t.Run(status, func(t *testing.T) {
			fc := newFakeClient()
			fc.listServersResult = []servers.Server{
				{ID: "server-1", Status: status, Metadata: map[string]string{MetadataKey: "test-asg"}},
			}
			g := newTestGroup(fc, nil)

			calls, err := collectUpdates(g, t)
			require.NoError(t, err)
			assert.Equal(t, []string{"server-1"}, fc.deletedServers())
			assert.Equal(t, []updateCall{{"server-1", provider.StateDeleting}}, calls)
		})
	}
}

// A transient status means Nova is working on the instance and it is
// expected back in ACTIVE. Report it as still creating so the runner stops
// scheduling onto it, but do NOT destroy a machine that is coming back.
func TestUpdate_TransientStatusesAreNotDeleted(t *testing.T) {
	for _, status := range []string{
		"BUILD", "MIGRATING", "PAUSED", "REBUILD",
		"REBOOT", "HARD_REBOOT", "RESIZE", "VERIFY_RESIZE", "REVERT_RESIZE", "RESCUE", "PASSWORD",
	} {
		t.Run(status, func(t *testing.T) {
			fc := newFakeClient()
			fc.listServersResult = []servers.Server{
				{ID: "server-1", Status: status, Metadata: map[string]string{MetadataKey: "test-asg"}},
			}
			g := newTestGroup(fc, nil)

			calls, err := collectUpdates(g, t)
			require.NoError(t, err)
			assert.Empty(t, fc.deletedServers(), "a recoverable instance must not be destroyed")
			assert.Equal(t, []updateCall{{"server-1", provider.StateCreating}}, calls)
		})
	}
}

// A status this plugin has never seen is reported as still creating rather
// than deleted — destroying a machine we don't understand is the worse
// failure — but it must not pass silently.
func TestUpdate_UnrecognizedStatusIsReportedNotDeleted(t *testing.T) {
	fc := newFakeClient()
	fc.listServersResult = []servers.Server{
		{ID: "server-1", Status: "SOME_FUTURE_STATUS", Metadata: map[string]string{MetadataKey: "test-asg"}},
	}
	g := newTestGroup(fc, nil)

	calls, err := collectUpdates(g, t)
	require.NoError(t, err)
	assert.Empty(t, fc.deletedServers())
	assert.Equal(t, []updateCall{{"server-1", provider.StateCreating}}, calls)
}
