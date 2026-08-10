# Folder Structure Guide

```
beuvian/
├── PROJECT.md                  Product specification — the source of truth
├── README.md
├── CONTRIBUTING.md
├── LICENSE                     MIT
├── Makefile                    Task runner (see `make help`)
├── go.work                     Go workspace: shared + backend + agent
├── .golangci.yml               Lint config, shared by all three modules
├── .env.example                Environment template
├── .dockerignore               Root-level: the backend image builds from here
│
├── shared/                     Wire protocol + cross-cutting libs. ZERO deps.
├── backend/                    API + WebSocket gateway (Go + Fiber)
├── agent/                      Desktop Agent (Go)
├── dashboard/                  Next.js dashboard (Phase 4)
│
├── docs/                       Documentation and ADRs
├── docker/                     Container definitions
├── infra/                      Deployment manifests
├── scripts/                    Build and dev scripts
└── .github/workflows/          CI and release
```

Every Go module compiles independently — CI proves it with `GOWORK=off`.

---

## `shared/` — the contract between the two binaries

```
shared/
├── go.mod                      NO require block. Enforced by CI.
├── protocol/                   The wire format
│   ├── protocol.go             Envelope, 13 message types, version, timing
│   ├── payloads.go             One struct per message payload
│   └── state.go                AgentState machine + ErrorCode
├── config/                     CLI > Env > File > Defaults engine
│   ├── config.go               Reflection-driven layer application
│   └── resolve.go              Two-pass orchestration, Describe, Usage
├── log/logger.go                slog JSON logging + secret redaction
├── lifecycle/lifecycle.go       Ordered startup, reverse-order shutdown
├── retry/backoff.go             Exponential backoff with jitter
├── id/id.go                     ULID-compatible sortable IDs
└── version/version.go           Build metadata via -ldflags
```

**Why this module exists at all:** the agent and backend must agree on the protocol
exactly. Sharing Go types rather than a schema means a payload change breaks
compilation on both sides immediately, instead of failing at runtime in production.

**Why it has no dependencies:** anything here lands in *both* binaries. See
[ADR-0003](adr/0003-shared-module-is-protocol-only.md).

**What must never go here:** business logic. If the backend and the agent both need
a rule, that is a sign the rule belongs to one of them and the other should be told
the outcome over the protocol.

---

## `backend/` — Clean Architecture layers

```
backend/
├── go.mod                      requires shared + yaml.v3
├── config.example.yaml
├── cmd/server/main.go          Wiring only. No business logic.
├── migrations/                 Versioned SQL (Phase 2)
├── seed/                       Seed data (Phase 2)
└── internal/
    ├── config/                 Schema + validation (no loading logic)
    │   ├── config.go
    │   └── validate.go
    ├── domain/                 Entities and rules. Imports no drivers.
    ├── port/                   Interfaces the app layer depends on
    ├── app/                    Use cases
    └── adapter/                Everything that touches the outside world
        ├── http/               Fiber routing, handlers, middleware, DTOs
        ├── ws/                 WebSocket gateway and connection hub
        ├── postgres/           Supabase PostgreSQL
        ├── redis/              Upstash Redis (ephemeral only)
        └── oauth/              GitHub OAuth
```

Dependency direction is `adapter → port ← app → domain`, enforced by review and
documented in each layer's `doc.go`.

`internal/` is a Go language feature, not a convention: nothing outside
`backend/` can import these packages, so the layering cannot be bypassed by an
external consumer.

**Where to add code:**

| Task | Location |
| --- | --- |
| New REST endpoint | `adapter/http/` → calls a use case in `app/` |
| New business rule | `domain/` if it is invariant, `app/` if it is orchestration |
| New table access | interface in `port/`, implementation in `adapter/postgres/` |
| New WebSocket message | `shared/protocol/` first, then `adapter/ws/` |
| New setting | `internal/config/config.go` + validation |

---

## `agent/` — the Desktop Agent

```
agent/
├── go.mod
├── config.example.yaml
├── cmd/beuvian-agent/main.go   Wiring + -detect diagnostic
└── internal/
    ├── config/                 Schema + validation
    ├── coding/                 THE extension point
    │   ├── adapter.go          Adapter + Detector interfaces
    │   ├── registry.go         Name → factory, capability reporting
    │   └── placeholder.go      Claude/Codex/Gemini/Aider/OpenHands
    ├── power/                  Sleep inhibition, per-OS
    │   ├── power.go            Interface + shared state tracking
    │   ├── power_windows.go    //go:build windows
    │   ├── power_darwin.go     //go:build darwin
    │   ├── power_linux.go      //go:build linux
    │   └── power_other.go      //go:build !windows && !darwin && !linux
    ├── session/                Lifecycle coordinator (Phase 3)
    ├── transport/              WebSocket client + reconnect (Phase 3)
    └── store/                  Encrypted local state (Phase 3)
```

**`coding/` is the most important package in the repository** for extensibility.
Everything else depends on the `Adapter` interface, never on a concrete adapter.
Adding Codex CLI is one file plus one `Register` call — if it requires touching
`session/` or `transport/`, the abstraction has leaked.

**`power/` uses build tags**, not a `runtime.GOOS` switch, so each target compiles
only the code that can run on it. CI cross-compiles and vets all six OS/arch
combinations because a Windows-only error in `power_windows.go` would otherwise
reach a release.

**`power_other.go` exists** so `GOOS=freebsd go build ./...` reports "untested
platform" rather than an undefined-symbol error.

---

## `docker/`

```
docker/
├── backend.Dockerfile          Multi-stage → distroless, non-root
└── docker-compose.yml          Local Postgres + Redis + backend
```

**Build from the repository root**, because the image needs `backend/` and
`shared/`:

```bash
docker build -f docker/backend.Dockerfile -t beuvian-backend:dev .
```

`.dockerignore` is at the root for the same reason.

**No agent Dockerfile, deliberately.** The agent must see the host's PATH, the
user's repositories, and the host power APIs. Containerising it would defeat its
purpose.

---

## `scripts/`

```
scripts/
├── build-agent.ps1             Windows host
└── build-agent.sh              macOS / Linux host
```

Two scripts rather than one because contributors are on all three platforms and no
single shell is present everywhere. Both produce byte-identical artifacts from the
same commit: version metadata comes from the commit date, not the wall clock, so
rebuilding reproduces the same binary.

---

## `docs/`

```
docs/
├── ARCHITECTURE.md             Layers, boundaries, data flow
├── DEVELOPER_GUIDE.md          Setup, workflow, conventions
├── WEBSOCKET_PROTOCOL.md       The realtime contract
├── API.md                      REST surface
├── DATABASE.md                 Schema and reasoning
├── DEPLOYMENT.md               Railway, Supabase, Upstash, Vercel
├── FOLDER_STRUCTURE.md         This file
├── TROUBLESHOOTING.md
└── adr/                        Architecture Decision Records
```

ADRs record decisions that had real alternatives, with the reasoning and the
tradeoff accepted. When a future reader asks "why on earth is it done this way",
the ADR is the answer — and if the reasoning no longer holds, that is grounds to
supersede it.

---

## `infra/`

```
infra/
├── railway/                    Backend service config
└── supabase/                   Project notes and SQL helpers
```

Deployment is configured through each platform's dashboard and environment
variables; this directory holds what can meaningfully be version-controlled. See
[DEPLOYMENT.md](DEPLOYMENT.md).

---

## Files that are generated, not authored

| Path | Origin |
| --- | --- |
| `dist/` | build scripts |
| `graphify-out/` | `make graph` |
| `go.work.sum` | Go toolchain (gitignored; `go.work` **is** committed) |
| `*/coverage.out` | `make test-cover` |
| `node_modules/`, `.next/` | npm (Phase 4) |

## Files that must never be committed

`.env` · `config.yaml` · `*.pem` · `*.key` · `agent.state`

All are in [`.gitignore`](../.gitignore) and [`.dockerignore`](../.dockerignore).
The `.example` counterparts **are** committed. Anything copied into a Docker build
context is recoverable from the image layers even if a later stage deletes it, which
is why secrets are excluded from the context rather than cleaned up inside it.
