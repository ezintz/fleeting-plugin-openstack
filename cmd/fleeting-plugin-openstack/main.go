// Command fleeting-plugin-openstack serves the OpenStack fleeting provider
// over the go-plugin gRPC protocol for GitLab Runner's docker-autoscaler.
package main

import (
	osplugin "github.com/ezintz/fleeting-plugin-openstack"
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"
)

func main() {
	// plugin.Main wraps Serve with the version/bootstrap CLI GitLab's
	// container-based plugin installation flow expects: `bootstrap <repo>`
	// installs this binary into the shared plugin directory image, and
	// `version`/-version report build metadata. With no arguments (how the
	// docker-autoscaler executor invokes the binary today) it falls through
	// to Serve, same as before.
	plugin.Main(&osplugin.InstanceGroup{}, plugin.VersionInfo{
		Name:      "fleeting-plugin-openstack",
		Version:   osplugin.Version,
		Revision:  osplugin.Revision,
		Reference: osplugin.Branch,
		BuiltAt:   osplugin.BuildDate,
	})
}
