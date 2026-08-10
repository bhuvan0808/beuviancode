module github.com/bhuvan0808/beuviancode/backend

go 1.26

require (
	github.com/bhuvan0808/beuviancode/shared v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

// The shared module is never published to a registry, so the local path must be
// declared here as well as in go.work. go.work covers day-to-day development;
// this replace covers the cases where the workspace is not in play — Docker
// builds, GOWORK=off in CI, and `go build` from inside this directory alone.
// See docs/adr/0002-go-workspace-multi-module.md.
replace github.com/bhuvan0808/beuviancode/shared => ../shared
