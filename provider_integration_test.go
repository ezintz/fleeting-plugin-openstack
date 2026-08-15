package fpoc

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"gitlab.com/gitlab-org/fleeting/fleeting/integration"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// TestProvisioning is GitLab fleeting's black-box acceptance suite: it builds
// the real plugin binary and drives it through a fleeting.Provisioner exactly
// as the runner would (scale up, scale down, SSH in and run a command, scale
// to zero). Unlike a public-cloud plugin, there is no image/flavor/network
// that exists in every OpenStack tenant, so this only runs against a real
// cloud an operator points it at via environment variables — everywhere else
// (including CI) it skips.
func TestProvisioning(t *testing.T) {
	if os.Getenv("OS_CLOUD") == "" && os.Getenv("OS_AUTH_URL") == "" {
		t.Skip("neither OS_CLOUD nor OS_AUTH_URL set; skipping live-cloud integration test")
	}

	env := map[string]string{
		"FLEETING_TEST_IMAGE_NAME":   "",
		"FLEETING_TEST_FLAVOR_NAME":  "",
		"FLEETING_TEST_NETWORK_ID":   "",
		"FLEETING_TEST_KEY_NAME":     "",
		"FLEETING_TEST_SSH_KEY_FILE": "",
		"FLEETING_TEST_SSH_USERNAME": "",
	}
	for name := range env {
		value := os.Getenv(name)
		if value == "" {
			t.Skipf("%s not set; skipping live-cloud integration test", name)
		}
		env[name] = value
	}

	sshKey, err := os.ReadFile(env["FLEETING_TEST_SSH_KEY_FILE"])
	if err != nil {
		t.Fatalf("reading FLEETING_TEST_SSH_KEY_FILE: %v", err)
	}

	pluginBinary := integration.BuildPluginBinary(t, "./cmd/fleeting-plugin-openstack", "fleeting-plugin-openstack")

	integration.TestProvisioning(t, pluginBinary, integration.Config{
		PluginConfig: InstanceGroup{
			Name: "fleeting-integration-test-" + randomSuffix(t),
			ServerSpec: ExtCreateOpts{
				CreateOpts: servers.CreateOpts{Name: "fleeting-integration-test-%d"},
				ImageName:  env["FLEETING_TEST_IMAGE_NAME"],
				FlavorName: env["FLEETING_TEST_FLAVOR_NAME"],
				KeyName:    env["FLEETING_TEST_KEY_NAME"],
				Networks: []PluginNetwork{
					{UUID: env["FLEETING_TEST_NETWORK_ID"]},
				},
			},
			BootTimeS: "3m",
		},
		ConnectorConfig: provider.ConnectorConfig{
			UseStaticCredentials: true,
			Username:             env["FLEETING_TEST_SSH_USERNAME"],
			Key:                  sshKey,
			Timeout:              10 * time.Minute,
		},
		MaxInstances:    1,
		UseExternalAddr: os.Getenv("FLEETING_TEST_USE_EXTERNAL_ADDR") == "true",
	})
}

func randomSuffix(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generating random suffix: %v", err)
	}

	return hex.EncodeToString(buf)
}
