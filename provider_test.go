package fpoc

import (
	"context"
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

	id1, err := g.createInstance(context.Background())
	assert.NoError(err)
	assert.Equal("server-1", id1)
	require.Len(t, fc.createPortCalls, 1, "first call should pre-create one port")
	assert.Equal("subnet-uuid", fc.createPortCalls[0].SubnetID)

	id2, err := g.createInstance(context.Background())
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

	id, err := g.createInstance(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "server-1", id)
	assert.Empty(t, fc.createPortCalls, "no SubnetID -> no port pre-creation")
	assert.Empty(t, fc.deletePortCalls, "nothing to clean up")
}

func TestCreateInstance_MixedNetworksOnlyPreCreatesForSubnetEntries(t *testing.T) {
	fc := newFakeClient()
	g := newTestGroup(fc, []PluginNetwork{
		{UUID: "net-a"},                           // Nova-managed, no port pre-creation
		{UUID: "net-b", SubnetID: "subnet-b"},     // pinned via pre-created port
		{UUID: "net-c", SubnetID: "subnet-c"},     // pinned via pre-created port
	})

	_, err := g.createInstance(context.Background())
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

	_, err := g.createInstance(context.Background())
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

	_, err := g.createInstance(context.Background())
	assert.NoError(t, err)
	require.Len(t, fc.createPortCalls, 1)
	assert.Nil(t, fc.createPortCalls[0].SecurityGroups,
		"when no security_groups are configured the plugin must not pass an empty list (which would mean 'attach no SGs at all')")
}

func TestCreateInstance_CleanupPortsOnServerFailure(t *testing.T) {
	fc := newFakeClient()
	fc.createServerErr = errors.New("nova exploded")

	g := newTestGroup(fc, []PluginNetwork{
		{UUID: "n", SubnetID: "s"},
	})

	id, err := g.createInstance(context.Background())
	assert.Error(t, err)
	assert.Empty(t, id)
	require.Len(t, fc.createPortCalls, 1, "port should still have been pre-created")
	assert.Equal(t, []string{"port-1"}, fc.deletePortCalls, "pre-created port must be cleaned up when CreateServer fails")
}
