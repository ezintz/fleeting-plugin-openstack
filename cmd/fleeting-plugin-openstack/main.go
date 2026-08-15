// Command fleeting-plugin-openstack serves the OpenStack fleeting provider
// over the go-plugin gRPC protocol for GitLab Runner's docker-autoscaler.
package main

import (
	osplugin "github.com/ezintz/fleeting-plugin-openstack"
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"
)

func main() {
	plugin.Serve(&osplugin.InstanceGroup{})
}
