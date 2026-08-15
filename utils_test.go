package fpoc

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsCloudInitFinished(t *testing.T) {
	testCases := []struct {
		name     string
		file     string
		readLen  int
		expected bool
	}{
		{"token-not-fond-1", "testdata/console_out.txt", 4096, false},
		{"finished-1", "testdata/console_out.txt", 102400, true},
		{"token-not-fond-2", "testdata/console_ubuntu2204.txt", 4096, false},
		{"finished-2", "testdata/console_ubuntu2204.txt", 102400, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := os.ReadFile(tc.file)
			require.NoError(t, err)

			var log string
			if len(buf) >= tc.readLen {
				log = string(buf[0:tc.readLen])
			} else {
				log = string(buf)
			}

			obtained := IsCloudInitFinished(log)
			assert.Equal(t, tc.expected, obtained)
		})
	}
}

func TestIsIgnitionFinished(t *testing.T) {
	testCases := []struct {
		name     string
		file     string
		readLen  int
		expected bool
	}{
		{"token-not-fond-1", "testdata/console_flatcar.txt", 4096, false},
		{"finished-1", "testdata/console_flatcar.txt", 102400, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := os.ReadFile(tc.file)
			require.NoError(t, err)

			var log string
			if len(buf) >= tc.readLen {
				log = string(buf[0:tc.readLen])
			} else {
				log = string(buf)
			}

			obtained := IsIgnitionFinished(log)
			assert.Equal(t, tc.expected, obtained)
		})
	}
}

func TestExtCreateOpts(t *testing.T) {
	assert := assert.New(t)

	cfgJSON := `
	{
		"name": "gitlab-runner-%d",
		"description": "podman instance",
		"imageRef": "f2403879-6fbe-49a0-b71f-54b70039f32a",
		"flavorRef": "5",
		"key_name": "gitlab-autoscaler",
		"networks": [{"uuid": "c487d046-80ad-4da0-8b98-4a48ad3c257a"}],
		"security_groups": ["allow_gitlab_runner"],
		"scheduler_hints": {"group": "a5b557be-b7f0-4cb3-8f7c-6b5092f29c2c"},
		"tags": ["podman", "CI"],
		"user_data": "#!cloud-config\npackage_update: true\npackage_upgrade: true\n",
		"metadata": {"foo": "bar"}
	}
	`

	expected := `{"server":{"description":"podman instance","flavorRef":"5","imageRef":"f2403879-6fbe-49a0-b71f-54b70039f32a","key_name":"gitlab-autoscaler","metadata":{"foo":"bar"},"name":"gitlab-runner-%d","networks":[{"uuid":"c487d046-80ad-4da0-8b98-4a48ad3c257a"}],"security_groups":[{"name":"allow_gitlab_runner"}],"tags":["podman","CI"],"user_data":"IyFjbG91ZC1jb25maWcKcGFja2FnZV91cGRhdGU6IHRydWUKcGFja2FnZV91cGdyYWRlOiB0cnVlCg=="}}`

	cfg := new(ExtCreateOpts)
	err := json.Unmarshal([]byte(cfgJSON), cfg)
	assert.NoError(err)

	assert.Equal("a5b557be-b7f0-4cb3-8f7c-6b5092f29c2c", cfg.SchedulerHints.Group)

	omap, err := cfg.ToServerCreateMap()
	assert.NoError(err)
	assert.NotNil(omap)

	req, err := json.Marshal(omap)
	assert.NoError(err)
	assert.Equal(expected, string(req))
}

func TestExtCreateOptsWithSubnetID(t *testing.T) {
	assert := assert.New(t)

	cfgJSON := `
	{
		"name": "gitlab-runner-%d",
		"flavorRef": "5",
		"imageRef": "f2403879-6fbe-49a0-b71f-54b70039f32a",
		"networks": [
			{"uuid": "c487d046-80ad-4da0-8b98-4a48ad3c257a", "subnet_id": "a1b2c3d4-0000-1111-2222-333344445555"}
		]
	}
	`

	cfg := new(ExtCreateOpts)
	err := json.Unmarshal([]byte(cfgJSON), cfg)
	assert.NoError(err)

	// Verify subnet_id is parsed into the PluginNetwork struct
	assert.Len(cfg.Networks, 1)
	assert.Equal("c487d046-80ad-4da0-8b98-4a48ad3c257a", cfg.Networks[0].UUID)
	assert.Equal("a1b2c3d4-0000-1111-2222-333344445555", cfg.Networks[0].SubnetID)

	// Simulate what the provider does: replace the network with a pre-created port
	cfg.Networks[0] = PluginNetwork{Port: "pre-created-port-id"}

	omap, err := cfg.ToServerCreateMap()
	assert.NoError(err)
	assert.NotNil(omap)

	req, err := json.Marshal(omap)
	assert.NoError(err)

	expected := `{"server":{"flavorRef":"5","imageRef":"f2403879-6fbe-49a0-b71f-54b70039f32a","name":"gitlab-runner-%d","networks":[{"port":"pre-created-port-id"}]}}`
	assert.Equal(expected, string(req))
}

func TestPluginNetworkJSON(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected PluginNetwork
	}{
		{
			"uuid only",
			`{"uuid": "net-id"}`,
			PluginNetwork{UUID: "net-id"},
		},
		{
			"uuid with subnet_id",
			`{"uuid": "net-id", "subnet_id": "sub-id"}`,
			PluginNetwork{UUID: "net-id", SubnetID: "sub-id"},
		},
		{
			"port only",
			`{"port": "port-id"}`,
			PluginNetwork{Port: "port-id"},
		},
		{
			"uuid with fixed_ip",
			`{"uuid": "net-id", "fixed_ip": "10.0.0.5"}`,
			PluginNetwork{UUID: "net-id", FixedIP: "10.0.0.5"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var net PluginNetwork
			err := json.Unmarshal([]byte(tc.input), &net)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, net)
		})
	}
}

func TestInsertSSHKeyIgn(t *testing.T) {
	testCases := []struct {
		name     string
		userData string
		expected string
	}{
		{"empty", "", `{"ignition":{"config":{"replace":{"verification":{}}},"proxy":{},"security":{"tls":{}},"timeouts":{},"version":"3.4.0"},"kernelArguments":{},"passwd":{"users":[{"name":"test","sshAuthorizedKeys":["testkey"]}]},"storage":{},"systemd":{}}`},
		{"diff-user", `{"ignition":{"version":"3.3.0"},"passwd":{"users":[{"name":"test2"}]}}`, `{"ignition":{"config":{"replace":{"verification":{}}},"proxy":{},"security":{"tls":{}},"timeouts":{},"version":"3.4.0"},"kernelArguments":{},"passwd":{"users":[{"name":"test2"},{"name":"test","sshAuthorizedKeys":["testkey"]}]},"storage":{},"systemd":{}}`},
		{"same-user", `{"ignition":{"version":"3.2.0"},"passwd":{"users":[{"name":"test","sshAuthorizedKeys":["testkey1"]}]}}`, `{"ignition":{"config":{"replace":{"verification":{}}},"proxy":{},"security":{"tls":{}},"timeouts":{},"version":"3.4.0"},"kernelArguments":{},"passwd":{"users":[{"name":"test","sshAuthorizedKeys":["testkey1","testkey"]}]},"storage":{},"systemd":{}}`},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)

			spec := &ExtCreateOpts{
				UserData: tc.userData,
			}

			err := InsertSSHKeyIgn(spec, "test", "testkey")
			assert.NoError(err)
			assert.Equal(tc.expected, spec.UserData)
		})
	}
}

// selectConnectAddress must be deterministic: Go deliberately randomizes map
// iteration order, so running the same input repeatedly is how a regression
// to "whichever address the range loop visits last" would actually show up.
func TestSelectConnectAddress_Deterministic(t *testing.T) {
	netAddrs := map[string][]Address{
		"tenant-a": {{Address: "10.0.0.5", Version: 4, Type: "fixed"}},
		"tenant-b": {{Address: "10.0.1.5", Version: 4, Type: "fixed"}},
		"tenant-c": {{Address: "10.0.2.5", Version: 4, Type: "fixed"}},
		"tenant-d": {{Address: "10.0.3.5", Version: 4, Type: "fixed"}},
	}

	first := selectConnectAddress(netAddrs)
	for i := 0; i < 50; i++ {
		require.Equal(t, first, selectConnectAddress(netAddrs), "must return the same address on every call for the same input")
	}
}

func TestSelectConnectAddress_PrefersFloatingOverFixed(t *testing.T) {
	netAddrs := map[string][]Address{
		"tenant": {{Address: "10.0.0.5", Version: 4, Type: "fixed"}},
		"public": {{Address: "203.0.113.9", Version: 4, Type: "floating"}},
	}

	assert.Equal(t, "203.0.113.9", selectConnectAddress(netAddrs))
}

func TestSelectConnectAddress_FloatingBeatsFixedRegardlessOfIPVersion(t *testing.T) {
	netAddrs := map[string][]Address{
		"tenant": {{Address: "10.0.0.5", Version: 4, Type: "fixed"}},
		"public": {{Address: "2001:db8::1", Version: 6, Type: "floating"}},
	}

	assert.Equal(t, "2001:db8::1", selectConnectAddress(netAddrs), "reachability (floating) outranks IP version preference")
}

func TestSelectConnectAddress_PrefersIPv4OverIPv6WhenTypeTies(t *testing.T) {
	netAddrs := map[string][]Address{
		"dualstack": {
			{Address: "2001:db8::1", Version: 6, Type: "fixed"},
			{Address: "10.0.0.5", Version: 4, Type: "fixed"},
		},
	}

	assert.Equal(t, "10.0.0.5", selectConnectAddress(netAddrs))
}

func TestSelectConnectAddress_TieBreaksByNetworkThenAddress(t *testing.T) {
	netAddrs := map[string][]Address{
		"net-b": {{Address: "10.0.1.5", Version: 4, Type: "fixed"}},
		"net-a": {{Address: "10.0.0.9", Version: 4, Type: "fixed"}},
	}

	assert.Equal(t, "10.0.0.9", selectConnectAddress(netAddrs), "lowest network name wins when rank ties")
}

func TestSelectConnectAddress_EmptyReturnsEmptyString(t *testing.T) {
	assert.Equal(t, "", selectConnectAddress(nil))
	assert.Equal(t, "", selectConnectAddress(map[string][]Address{}))
}
