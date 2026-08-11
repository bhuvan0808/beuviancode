module github.com/bhuvan0808/beuviancode/backend

go 1.26

require (
	github.com/bhuvan0808/beuviancode/shared v0.0.0
	github.com/gofiber/contrib/websocket v1.3.4
	github.com/gofiber/fiber/v2 v2.52.14
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/jackc/pgx/v5 v5.10.0
	github.com/redis/go-redis/v9 v9.22.0
	golang.org/x/oauth2 v0.36.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/andybalholm/brotli v1.1.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/fasthttp/websocket v1.5.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.16 // indirect
	github.com/rivo/uniseg v0.2.0 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/savsgio/gotils v0.0.0-20240303185622-093b76447511 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.52.0 // indirect
	github.com/valyala/tcplisten v1.0.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/net v0.33.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

// The shared module is never published to a registry, so the local path must be
// declared here as well as in go.work. go.work covers day-to-day development;
// this replace covers the cases where the workspace is not in play — Docker
// builds, GOWORK=off in CI, and `go build` from inside this directory alone.
// See docs/adr/0002-go-workspace-multi-module.md.
replace github.com/bhuvan0808/beuviancode/shared => ../shared
