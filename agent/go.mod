module github.com/bhuvan0808/beuviancode/agent

go 1.26

require (
	github.com/bhuvan0808/beuviancode/shared v0.0.0
	github.com/gorilla/websocket v1.5.3
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/kr/pretty v0.3.0 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

// See backend/go.mod for why this mirrors go.work.
replace github.com/bhuvan0808/beuviancode/shared => ../shared
