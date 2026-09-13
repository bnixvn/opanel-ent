// Package version carries build metadata stamped in by the linker.
package version

import "runtime/debug"

// Set with -ldflags "-X github.com/bnixvn/opanel-ent/internal/version.Version=..."
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// String renders a one-line build identifier for logs and --version output.
func String() string {
	s := Version
	if Commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, kv := range bi.Settings {
				if kv.Key == "vcs.revision" {
					Commit = kv.Value
				}
			}
		}
	}
	if Commit != "" {
		if len(Commit) > 12 {
			Commit = Commit[:12]
		}
		s += "+" + Commit
	}
	if Date != "" {
		s += " (" + Date + ")"
	}
	return s
}
