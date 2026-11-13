// Package version holds the build metadata, set through ldflags.
package version

import "runtime/debug"

// Set via -ldflags "-X github.com/stuttgart-things/machinery-order-service/internal/version.Version=…".
var (
	Version   = ""
	Commit    = ""
	BuildDate = ""
)

// Info is the build metadata; empty values fall back to the module's build
// info.
func Info() map[string]string {
	v, c, d := Version, Commit, BuildDate
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v == "" && bi.Main.Version != "" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			}
		}
	}
	if v == "" {
		v = "dev"
	}
	return map[string]string{"version": v, "commit": c, "buildDate": d}
}
