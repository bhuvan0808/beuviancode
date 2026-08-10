# Developer Guide

## Prerequisites

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | 1.26+ | backend, agent, shared |
| Node.js | 20+ | dashboard (Phase 4) |
| Docker | 24+ | local Postgres and Redis (optional) |
| Git | 2.40+ | |
| `make` | any | task runner — Windows users see below |

Windows without `make`: use the PowerShell scripts in [`scripts/`](../scripts/)
directly, or run `make` under Git Bash or WSL.

## First run

```bash
git clone https://github.com/bhuvan0808/beuviancode.git
cd beuvian
make check      # build + vet + test + format check, every module
```

That should pass on a clean clone with **no configuration at all**. If it does not,
that is a bug worth reporting — the defaults are deliberately chosen so a fresh
clone works.

```bash
make detect         # which coding agents are installed here
make config-check   # validate both binaries' configuration
make help           # every available task
```

## The Go workspace

Three modules with a `go.work` at the root:

```
go.work → ./shared  ./backend  ./agent
```

The workspace is for day-to-day development. **Every Go command in `make` and CI
runs with `GOWORK=off`**, so resolution goes through each module's `replace`
directive instead. That is not pedantry: building through the workspace can succeed
while a clean single-module clone fails, and running with the workspace off locally
catches that before CI does.

If you add a module, add it to `go.work` **and** add the matching `replace`
directive. `make verify-modules-standalone` proves each one still stands alone.

### The shared module has no dependencies

`shared/go.mod` must contain no `require` block. It is imported by both binaries, so
anything added there lands in both — the agent must not carry Fiber, the backend
must not carry OS power syscalls. `make verify-shared-deps` and a CI job both
enforce it. See [ADR-0003](adr/0003-shared-module-is-protocol-only.md).

If you genuinely need a dependency in shared code, the answer is almost always to
invert it: take the thing you need as a function or interface parameter, as
`shared/config` does with its file decoder.

## Configuration

Resolution order is `CLI > Environment > Config file > Defaults`.

```bash
# defaults only
go run ./cmd/server

# config file
cp backend/config.example.yaml backend/config.yaml
go run ./cmd/server -config backend/config.yaml

# environment (upper-case the path, prefix BEUVIAN_)
BEUVIAN_SERVER_PORT=9000 go run ./cmd/server

# CLI wins over everything
BEUVIAN_SERVER_PORT=9000 go run ./cmd/server -port 7000   # → 7000
```

Adding a setting is one struct field:

```go
// backend/internal/config/config.go
type Server struct {
    MaxUploadMB int `yaml:"max_upload_mb" env:"MAX_UPLOAD_MB" flag:"max-upload" default:"10" usage:"upload cap in MB"`
}
```

Tags: `env`, `flag`, `default`, `usage`, `secret:"true"`. A `secret` field is
redacted from `Describe()` output. Nested structs compose their `env` prefixes, so
`Server` with `env:"SERVER"` plus a field with `env:"MAX_UPLOAD_MB"` yields
`BEUVIAN_SERVER_MAX_UPLOAD_MB`.

Then add validation in `validate.go`. Validation is **aggregated** — return every
problem rather than the first, and prefer messages that name the offending variable
and say what to do.

## Local infrastructure

```bash
make infra      # Postgres + Redis only; run the backend from source
make up         # the whole stack in containers
make logs
make down       # stop, keep data
make reset      # stop and DELETE local volumes (prompts first)
```

Local Postgres and Redis stand in for Supabase and Upstash. Both are
wire-compatible, so application code is identical against either — a developer must
not need cloud credentials to work on Beuvian.

## Testing

```bash
make test              # all modules, race detector on
make test-cover        # with coverage summary
cd shared && go test -race ./protocol/
cd shared && go test -race -run TestPrecedenceOrder ./config/
```

`-race` is not optional here. The gateway, the adapter registry, and the lifecycle
supervisor are all concurrent, and a data race in any of them is precisely the kind
of bug that only appears under production load.

### What a good test looks like in this codebase

Tests assert **behaviour that matters**, and say why in a comment. Compare:

```go
// Weak: restates the implementation.
if b.Next() != 100*time.Millisecond { t.Error("wrong") }

// Useful: states the consequence of being wrong.
// Regression guard: a large attempt count overflows the float->Duration
// conversion, which produced a negative delay and an immediate retry storm.
for i := 0; i < 500; i++ {
    if d := b.Next(); d < 0 { t.Fatalf("attempt %d produced %v", i, d) }
}
```

Several existing tests are deliberately **tripwires** — they fail when an
assumption changes, forcing a decision rather than letting it drift:

- `TestMessageTypeSetMatchesSpec` fails if the protocol gains a type without the
  spec and docs being updated.
- `TestImplementedIsHonestAboutPhase1` fails when the first real adapter lands, so
  the claim "no adapter is implemented" cannot silently become false.
- `TestHeartbeatTimeoutToleratesOneMissedBeat` fails if someone tightens the
  timeout below 2× the interval.

Table-driven tests are the default. Use `t.Setenv` (not `os.Setenv`) so the
environment is restored, and `t.TempDir()` for files.

## Layer boundaries in the backend

```
adapter ──▶ port ◀── app ──▶ domain
```

The rules that matter:

- `domain` imports nothing from the backend. No SQL, no HTTP, no drivers.
- `app` may import `domain` and `port`, never `adapter`. A use case that imports a
  driver cannot be tested without that driver running.
- Interfaces live in `port`, beside their **consumer**, not their implementation.
- Adapters define their own request/response DTOs rather than serialising domain
  entities — the wire format is a public contract, and marshalling entities directly
  makes every internal rename a breaking API change.

Each layer's `doc.go` states its contract. Read it before adding to that package.

## Adding a coding agent adapter

1. Implement `coding.Adapter` in `agent/internal/coding/`.
2. Add the executable names to `knownAdapters()`.
3. Add the name to `Implemented()`.
4. Register it in `RegisterPlaceholders` (or replace the placeholder factory).

That is the whole change. Nothing in `session`, `transport`, or `main` needs
touching — if it does, the abstraction has leaked and that is the bug to fix.

## Code style

`gofmt` is enforced; `make fmt` fixes it. `golangci-lint` config is at
[`.golangci.yml`](../.golangci.yml).

Conventions this codebase actually holds to:

- **Comments explain *why*, not *what*.** The code says what. If a comment restates
  the line below it, delete it. If a value was chosen for a reason (a timeout, a
  buffer size, a default), say what breaks if it changes.
- **Errors wrap with `%w`** and are compared with `errors.Is`/`errors.As`, never by
  string. Sentinel errors are part of a package's contract.
- **No package-level mutable state.** Dependencies come through constructors. The
  adapter registry is an instance for exactly this reason.
- **Contexts on anything that does I/O**, and honour cancellation.
- **Don't call `os.Exit` outside `main`.** It skips deferred cleanup. Return an
  error and let `main` decide — `log.Fatalf` in this codebase returns an error
  rather than exiting for the same reason.
- **Never log a credential.** `shared/log` redacts secret-shaped keys as a backstop,
  but do not rely on it.

## The knowledge graph

The repository ships a [graphify](https://pypi.org/project/graphifyy/) knowledge
graph, useful for orienting in the codebase and for asking structural questions
without reading every file:

```bash
pip install graphifyy && graphify install

make graph                                    # rebuild (AST only — no LLM tokens)
graphify query "how does configuration precedence work"
graphify path "Adapter" "Registry"
graphify explain "Supervisor"
```

For a code-only corpus graphify extracts structurally with tree-sitter and uses no
model at all, so rebuilding is free. Outputs land in `graphify-out/`
(`graph.html` is an interactive view).

## Common tasks

| | |
| --- | --- |
| Run backend | `make run-backend` |
| Run agent | `make run-agent` |
| Validate config | `make config-check` |
| List installed coding agents | `make detect` |
| Build agent for this platform | `make build-agent` |
| Build all six release targets | `make build-agent-all` |
| Build backend image | `make docker-build` |
| Everything CI checks | `make check` |
| Prove modules stand alone | `make verify-modules-standalone` |
| Vulnerability scan | `make vuln` |
