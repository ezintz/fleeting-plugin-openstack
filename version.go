package fpoc

import (
	"fmt"
	"runtime"
)

// Build metadata, reported to the runner through provider.ProviderInfo.
// The release build overrides these with -ldflags -X; see
// .slsa-goreleaser/linux-*.yml.
var (
	Version   = "dev"
	Revision  = "HEAD"
	Branch    = "HEAD"
	BuildUser = "nobody"
	BuildDate = "now"
)

// BuildInfo renders the build metadata as the single-line summary the
// runner logs when the plugin is initialized.
func BuildInfo() string {
	return fmt.Sprintf("sha=%s; ref=%s; go=%s; built_at=%s; os_arch=%s/%s",
		Revision, Branch, runtime.Version(), BuildDate, runtime.GOOS, runtime.GOARCH)
}
