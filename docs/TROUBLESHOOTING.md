# Troubleshooting Guide

## Start here

```bash
beuvian-agent -version    # which build is this?
beuvian-agent -detect     # is the coding agent installed?
beuvian-agent -check      # is the configuration valid?
```

For the backend, `-check` validates configuration and exits without starting
anything, and `-log-level debug` logs the effective configuration with secrets
redacted:

```bash
go run ./cmd/server -check
go run ./cmd/server -log-level debug   # look for "effective configuration"
```

When reporting a problem, include the output of `-version` and the `request_id` or
`correlation_id` from the failure. A correlation ID follows one action across the
dashboard, backend, and agent, which usually reduces a report to a single search.

---

## Build and toolchain

### `cannot load module ../agent listed in go.work file`

`go.work` references a module directory that does not exist, usually mid-checkout or
after moving a module.

```bash
go work sync
```

If a module was intentionally removed, remove it from `go.work` too.

### Builds work locally but fail in CI

Almost always the workspace. CI runs every Go command with `GOWORK=off`, so a module
that only compiles because a sibling is present on disk fails there. Reproduce it:

```bash
make verify-modules-standalone
# or, for one module:
cd backend && GOWORK=off go build ./...
```

The fix is normally a missing `replace` directive in that module's `go.mod`.
`go.work` covers development; the `replace` covers Docker, CI, and a clean
single-module clone.

### `shared/go.mod has gained a dependency`

Intentional. `shared` is imported by both binaries, so anything added there lands in
both. See [ADR-0003](adr/0003-shared-module-is-protocol-only.md).

The usual fix is to invert the dependency — take what you need as a parameter, the
way `shared/config` accepts a file decoder function instead of importing a YAML
library.

### `gofmt` failures in CI

```bash
make fmt
```

### `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`

The race detector needs a C toolchain, and Go sets `CGO_ENABLED=0` automatically when
it cannot find one. Check with:

```bash
go env CGO_ENABLED    # 0 means no C compiler was found
```

On Windows, install a C compiler ([MinGW-w64](https://www.mingw-w64.org/) or
[TDM-GCC](https://jmeubank.github.io/tdm-gcc/)) and make sure `gcc` is on `PATH`. On
macOS install the Xcode command-line tools; on Linux install `gcc`.

Until then, run tests without it:

```bash
cd shared && go test ./...
```

This is a **local** gap only. CI runs on `ubuntu-latest`, where `-race` is always
enabled — so a data race still gets caught before merge. It is worth fixing locally
anyway, because the gateway, adapter registry, and lifecycle supervisor are all
concurrent and the feedback loop is much faster on your own machine.

### Docker: `failed to connect to the docker API`

Docker Desktop is not running. On Windows, also confirm the Linux engine is selected
rather than Windows containers.

### Docker build: `shared/go.mod: no such file or directory`

The image is being built from `docker/` instead of the repository root. It needs both
`backend/` and `shared/`:

```bash
docker build -f docker/backend.Dockerfile -t beuvian-backend:dev .   # note the trailing dot
```

---

## Configuration

### `invalid backend configuration:` with several lines

Working as designed — every problem is reported at once rather than one per run,
because a fresh deployment usually has several settings missing and one-at-a-time
errors turn that into a guessing game. Fix them all and re-run.

### A setting is ignored

Check the precedence order: **CLI > Environment > Config file > Defaults**. A leftover
exported variable silently beats your config file.

```bash
env | grep BEUVIAN            # macOS/Linux
Get-ChildItem Env:BEUVIAN*    # PowerShell
```

Then confirm which file was actually loaded — the startup log reports it:

```
"config_file":"backend/config.yaml"
"config_file":"(defaults, env and flags only)"    ← no file was read
```

That second line is the answer to most "my config file is being ignored" reports.

### Environment variable has no effect

The name must compose the nesting. `server.port` inside a `Server` section tagged
`env:"SERVER"` becomes:

```
BEUVIAN_SERVER_PORT        ✅
BEUVIAN_PORT               ❌
```

Agent variables use `BEUVIAN_AGENT_`, not `BEUVIAN_`.

### Production refuses to start

Production validation is stricter on purpose. Each of these is a real security
defect that would otherwise run unnoticed:

| Error | Fix |
| --- | --- |
| `cookie_secure: must be true in production` | Do not disable it; fix the deployment so TLS terminates correctly |
| `is plaintext http; production requires https` | Use `https://` origins |
| `jwt_secret: N bytes is below the 32-byte minimum` | `openssl rand -base64 48` |
| `log.level: debug is not permitted in production` | Use `info` |
| `rate_limit.enabled requires redis.url` | Set `BEUVIAN_REDIS_URL`, or disable rate limiting and accept the consequence |

### `Auth.JWTSecret=<unset>` in debug output

`<unset>` means empty; a configured secret shows `<redacted>`. The distinction exists
because a missing secret is usually the actual bug.

---

## Agent

### `Claude Code was not found`

```bash
beuvian-agent -detect
```

Beuvian does not install or bundle Claude Code — you install it yourself and Beuvian
drives it. Confirm it is on `PATH`:

```bash
claude --version         # macOS/Linux
where.exe claude         # Windows
```

**On Windows**, the npm install creates `claude.cmd`, not `claude.exe`. Detection
looks for `claude`, `claude.cmd`, and `claude.exe`, so a working `claude` in a
terminal should be found. If the terminal works but detection does not, the usual
cause is that `PATH` was updated after the agent started — restart it.

Otherwise, point at it explicitly:

```yaml
coding:
  executable_path: C:\Users\you\AppData\Roaming\npm\claude.cmd
```

### `selected adapter is a placeholder and cannot run a session yet`

Expected in Phase 1 — no adapter is implemented yet. `codex`, `gemini`, `aider`, and
`openhands` are registered so they can be *detected* and reported, but they cannot
drive a session and will not in the MVP. Only `claude` is targeted, arriving in
Phase 3.

### `coding.working_directory: "…" does not exist`

Deliberately has no default. The coding agent writes files, so guessing this wrong
destroys real work. Set it explicitly, or leave it empty and choose the repository
from the dashboard.

### `coding.auto_start requires coding.working_directory`

`auto_start: true` with nothing to start in would fail at launch, after you believed
startup succeeded. Either set the directory or turn auto-start off.

### `sleep prevention is unavailable on this platform build`

Phase 1 ships the cross-platform structure; the syscalls arrive in Phase 3. The agent
warns rather than claiming an inhibition it is not holding. Until then, disable sleep
in your OS settings for long sessions.

### The agent keeps reconnecting

Look at the `ERROR` code in the logs:

- `unauthorized` / `device_not_found` — the device token is expired or revoked. **Not
  retryable**; re-register the device.
- `version_unsupported` — the agent is older than the backend supports. Upgrade it.
- `rate_limited` / `internal` — retryable; the agent backs off automatically.

An unbounded reconnect loop is by design: a laptop closed for the weekend must
reconnect on Monday. What must *not* loop forever is a rejected credential, which is
why non-retryable codes stop the loop.

### The agent goes offline while the machine is idle

Sleep inhibition is not implemented yet (Phase 3). A sleeping machine stops the coding
agent, which is precisely the failure Beuvian exists to prevent — and the reason the
power manager is not optional.

---

## Backend

### `no service components registered yet`

Expected in Phase 1. The process establishes configuration, logging, and supervised
lifecycle, then blocks on a signal. The HTTP server, database, Redis, and WebSocket
gateway arrive in Phase 2.

### Postgres: `too many connections` / `remaining connection slots are reserved`

`instances × max_open_conns` has exceeded Supabase's cap.

1. Use the **pooler endpoint (port 6543)**, not the direct one (5432).
2. Lower `BEUVIAN_DB_MAX_OPEN_CONNS`.

This failure mode is abrupt: it presents as every instance failing at once rather
than as gradual degradation.

### CORS errors in the browser

- The origin must be listed **exactly**, including scheme and port. A trailing slash
  mismatch is a common cause.
- `allow_credentials: true` is incompatible with a wildcard origin — browsers reject
  the combination, and validation rejects it at boot to save you the confusion.
- Cross-subdomain cookies need `auth.cookie_domain` set.

### GitHub OAuth: `redirect_uri_mismatch`

The callback URL must match the OAuth app **character for character**, including
scheme, host, port, and trailing path. Compare
`BEUVIAN_AUTH_GITHUB_CALLBACK_URL` against the app's setting rather than trusting
memory.

### Rate limiting is not working

If `redis.url` is empty, the limiter has nothing to enforce with. In development this
is allowed and warned about:

```
"rate limiting is enabled but Redis is absent; requests are NOT being limited"
```

Outside development it is a hard startup error, because a security control that looks
enabled but is not is worse than one that is plainly off.

### Shutdown takes too long / is killed mid-drain

`shutdown_grace` must stay under 30s — Railway sends `SIGKILL` 30s after `SIGTERM`, so
a longer grace is truncated rather than honoured. Validation enforces the cap.

If a component overruns, the logs name it:

```
"component exceeded its shutdown budget","component":"httpserver"
```

---

## Realtime

### Connects then immediately disconnects

The socket is unauthenticated until `AUTH` is answered with `ACK`, and anything sent
before that closes the connection. Check for an `ERROR` frame before the close — it
carries the reason.

### `replay_detected`

Either the clock is off by more than 2 minutes, or an `AUTH` nonce was reused. Every
`AUTH`, including on reconnect, needs a **fresh** nonce. If the clock is the cause,
enable NTP — this is common on a machine resuming from sleep.

### Logs arrive out of order or with gaps

Use `seq` to order and to detect gaps; it is a per-connection monotonic counter for
exactly this reason. `truncated: true` means the agent's ring buffer overflowed and
lines were genuinely dropped — the flag exists so a truncated transcript cannot be
mistaken for a complete one. Raise `session.log_buffer_lines` if it happens often.

### A dashboard tab stops receiving updates

Each connection has a bounded outbound queue. A consumer too slow to drain it is
disconnected rather than being allowed to block the broadcaster, because one stalled
phone must not freeze every other client. Reconnecting resumes updates.

---

## Windows specifics

### Vertical scrolling breaks after running `graphify`

A known issue with ANSI escape sequences from a graphify dependency. Use Windows
Terminal rather than the legacy console, or `pip install --upgrade graphifyy`.

### `make` is not recognised

`make` is not part of a stock Windows install. Use the PowerShell scripts directly:

```powershell
./scripts/build-agent.ps1 -Target All
```

Or run `make` under Git Bash or WSL.

### The build script fails with `fatal: not a git repository`

Fixed — git failures now degrade to `dev` version metadata with a warning. If you see
this on an older checkout, `git init` or update the script. The underlying cause is
that Windows PowerShell 5.1 converts a native command's stderr into a terminating
error under `$ErrorActionPreference = 'Stop'`.

---

## Still stuck

The knowledge graph can answer structural questions without reading the whole
codebase:

```bash
make graph
graphify query "how does the agent authenticate to the backend"
graphify path "PromptPayload" "prompt_queue"
```

When opening an issue, include:

1. `beuvian-agent -version` or the backend's startup log line
2. The `config_file` value from that log line
3. The `correlation_id` or `request_id` from the failure
4. Relevant log lines — secrets are already redacted by the logger
