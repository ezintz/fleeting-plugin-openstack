package fpoc

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Build metadata, reported to the runner through provider.ProviderInfo and to
// the CLI through plugin.Main. The release build sets Version, Revision and
// BuildUser with -ldflags -X; see .slsa-goreleaser/linux-*.yml.
//
// These deliberately start empty rather than at placeholders like "HEAD" or
// "now". plugin.Main and the init below both only fill in fields that are
// still empty, so a non-placeholder default would suppress the real values
// the Go toolchain already stamps into every VCS-aware build.
var (
	Version   string
	Revision  string
	Branch    string
	BuildUser string
	BuildDate string
)

// unknown is what BuildInfo reports for metadata no build stamped.
const unknown = "unknown"

// init fills whatever -ldflags did not set from the build information the Go
// toolchain embeds. It runs before plugin.Main's equivalent fallback, so the
// version the CLI prints and the BuildInfo the runner logs agree.
func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		if Version == "" {
			Version = info.Main.Version
		}

		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if Revision == "" {
					Revision = setting.Value
					if len(Revision) > 12 {
						Revision = Revision[:12]
					}
				}

			case "vcs.time":
				// RFC 3339, which is what plugin.Main's bootstrap
				// path needs to derive a pseudo-version.
				if BuildDate == "" {
					BuildDate = setting.Value
				}
			}
		}
	}

	// A module built from a checkout rather than a tagged release reports
	// itself as "(devel)", which is not a version anyone can act on.
	if Version == "" || Version == "(devel)" {
		Version = "dev"
	}

	// Unlike Version/Revision/BuildDate there is nothing in the build info
	// to recover a branch from, and plugin.Main has no fallback for it
	// either — it just prints whatever it is given. Name it rather than
	// leaving the CLI's "Git ref:" line blank.
	if Branch == "" {
		Branch = unknown
	}
}

// BuildInfo renders the build metadata as the single-line summary the
// runner logs when the plugin is initialized.
func BuildInfo() string {
	return fmt.Sprintf("sha=%s; ref=%s; go=%s; built_at=%s; os_arch=%s/%s",
		orUnknown(Revision), orUnknown(Branch), runtime.Version(),
		orUnknown(BuildDate), runtime.GOOS, runtime.GOARCH)
}

func orUnknown(s string) string {
	if s == "" {
		return unknown
	}

	return s
}
