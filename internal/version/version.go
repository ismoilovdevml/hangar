// Package version holds build information, set with -ldflags at build time:
//
//	-X github.com/ismoilovdevml/hangar/internal/version.Version=v0.1.0
//	-X github.com/ismoilovdevml/hangar/internal/version.Commit=abc1234
//	-X github.com/ismoilovdevml/hangar/internal/version.Date=2026-01-01T00:00:00Z
package version

import (
	"runtime"
	"runtime/debug"
)

// Set by the linker.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is build information, returned by `hangar version` and the API.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"go_version"`
}

// Get returns the build information, falling back to VCS data embedded by
// the Go toolchain when ldflags were not set.
func Get() Info {
	i := Info{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version()}
	if i.Commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if len(s.Value) > 12 {
						i.Commit = s.Value[:12]
					} else {
						i.Commit = s.Value
					}
				case "vcs.time":
					if i.Date == "" {
						i.Date = s.Value
					}
				}
			}
		}
	}
	return i
}

// UserAgent is the User-Agent Hangar sends upstream.
func UserAgent() string { return "Hangar/" + Version }
