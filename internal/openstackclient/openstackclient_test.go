package openstackclient

import (
	"os"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/testhelper"
	thclient "github.com/gophercloud/gophercloud/v2/testhelper/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetImageProperties(t *testing.T) {
	assert := assert.New(t)

	img, err := os.ReadFile("../../testdata/image_get.json")
	require.NoError(t, err)

	testhelper.SetupHTTP()
	defer testhelper.TeardownHTTP()

	testhelper.ServeFile(t, "", "", "application/json", string(img))

	client := &client{
		compute: thclient.ServiceClient(),
		image:   thclient.ServiceClient(),
	}

	ctx := t.Context()
	props, err := client.GetImageProperties(ctx, "1da9661c-953e-424d-a1e5-834a8174b198")
	assert.NoError(err)
	if assert.NotNil(props) {
		assert.Equal("core", props.OSAdminUser)
	}

	t.Log(props)
}

func TestGetImageByName(t *testing.T) {
	assert := assert.New(t)

	img, err := os.ReadFile("../../testdata/image_list_one.json")
	require.NoError(t, err)

	testhelper.SetupHTTP()
	defer testhelper.TeardownHTTP()

	testhelper.ServeFile(t, "", "", "application/json", string(img))

	client := &client{
		compute: thclient.ServiceClient(),
		image:   thclient.ServiceClient(),
	}

	ctx := t.Context()
	imageRef, props, err := client.GetImageByName(ctx, "flatcar_production_openstack_3815.2.5_amd64.raw")
	assert.NoError(err)
	assert.Equal("463074fa-f5cb-4601-b5da-5c45b9aa9981", imageRef)
	if assert.NotNil(props) {
		assert.Equal("core", props.OSAdminUser)
	}

	t.Log(props)
}

func TestGetImageByName_Many(t *testing.T) {
	assert := assert.New(t)

	img, err := os.ReadFile("../../testdata/image_list_many.json")
	require.NoError(t, err)

	testhelper.SetupHTTP()
	defer testhelper.TeardownHTTP()

	testhelper.ServeFile(t, "", "", "application/json", string(img))

	client := &client{
		compute: thclient.ServiceClient(),
		image:   thclient.ServiceClient(),
	}

	ctx := t.Context()
	_, _, err = client.GetImageByName(ctx, "flatcar")
	assert.ErrorIs(err, gophercloud.ErrMultipleResourcesFound{Name: "flatcar", Count: 8, ResourceType: "image"})
}

// env.Parse applies an envDefault by overwriting whatever the field already
// holds, and New calls it after Init has copied the plugin's
// nova_microversion in. An envDefault on ComputeAPIVersion therefore
// discarded that value whenever OS_COMPUTE_API_VERSION was unset — silently,
// with the operator's configured microversion never reaching Nova.
func TestHTTPOpts_MicroversionPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		envValue   string
		want       string
	}{
		{"nothing set falls back to the default", "", "", DefaultComputeAPIVersion},
		{"plugin config is honoured", "2.96", "", "2.96"},
		{"env var overrides the plugin config", "2.96", "2.90", "2.90"},
		{"env var alone is honoured", "", "2.90", "2.90"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envValue != "" {
				t.Setenv("OS_COMPUTE_API_VERSION", tc.envValue)
			} else {
				t.Setenv("OS_COMPUTE_API_VERSION", "")
				os.Unsetenv("OS_COMPUTE_API_VERSION")
			}

			cfg := &EnvCloudConfig{CloudConfig: CloudConfig{ComputeAPIVersion: tc.configured}}
			require.NoError(t, env.Parse(cfg))

			_, got := cfg.HTTPOpts()
			assert.Equal(t, tc.want, got)
		})
	}
}
