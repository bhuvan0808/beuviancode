module github.com/bhuvan0808/beuviancode/agent

go 1.26

require (
	github.com/bhuvan0808/beuviancode/shared v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

// See backend/go.mod for why this mirrors go.work.
replace github.com/bhuvan0808/beuviancode/shared => ../shared
