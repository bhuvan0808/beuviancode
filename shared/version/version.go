// Package version exposes build metadata injected at link time.
//
// Values are set with -ldflags by scripts/build-agent.* and the release workflow,
// so a shipped binary can report exactly which commit produced it. This is not a
// nicety: agents run on machines we cannot inspect, and a bug report is only
// actionable if the binary can state its own provenance.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Injected via -ldflags "-X github.com/bhuvan0808/beuviancode/shared/version.Version=...".
var (
	// Version is the semantic version, or "dev" for a local build.
	Version = "dev"
	// Commit is the full git SHA.
	Commit = "none"
	// Date is the RFC3339 build timestamp.
	Date = "unknown"
)

// Info is a structured view of the build, suitable for a /health response, a
// startup log line, or the AUTH handshake's agent_version field.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the current build info.
//
// When ldflags were not applied (go run, go test, or `go install` of a tagged
// module), it falls back to the module metadata the toolchain embeds
// automatically, so the reported version is still better than "dev".
func Get() Info {
	v, c := Version, Commit
	if v == "dev" || c == "none" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			if v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				v = bi.Main.Version
			}
			if c == "none" {
				for _, s := range bi.Settings {
					if s.Key == "vcs.revision" {
						c = s.Value
						break
					}
				}
			}
		}
	}
	return Info{
		Version:   v,
		Commit:    c,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// Short returns a one-line human-readable version string.
func Short() string {
	i := Get()
	commit := i.Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return fmt.Sprintf("%s (%s, %s)", i.Version, commit, i.Platform)
}

// UserAgent returns a User-Agent value identifying this build to the backend.
func UserAgent(component string) string {
	i := Get()
	return fmt.Sprintf("beuvian-%s/%s (%s)", component, i.Version, i.Platform)
}
