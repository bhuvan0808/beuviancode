# Architecture Guide

How Beuvian is put together, and why. Decisions with meaningful alternatives are
recorded as [ADRs](adr/); this document explains the system as a whole.

---

## 1. The problem being solved

A developer starts an AI coding agent on a task that takes 45 minutes, then leaves
the machine. Three things then go wrong:

1. **They cannot tell when it finished.** The signal exists only on the laptop.
2. **They cannot answer it.** Coding agents pause for confirmation or for the next
   instruction, and a paused agent is indistinguishable from a finished one.
3. **The machine may sleep**, stopping the agent entirely.

Beuvian solves exactly these. It is a remote control and observation layer over a
coding agent that continues to run locally, under the user's own account and
subscription.

### What Beuvian deliberately is not

Beuvian never calls a model API. It does not proxy prompts to Anthropic or OpenAI,
never holds provider credentials, and does not replace the coding agent.

This is an architectural constraint, not marketing. It means Beuvian carries no
inference cost, no model-provider dependency, and no liability for what the coding
agent does with its own credentials. It also means the product's value is entirely
in transport, state, and timing — which is why the protocol and the session state
machine get disproportionate attention below.

---

## 2. Component overview

```
   ┌─────────────────────── user's machine ────────────────────────┐
   │                                                               │
   │   ┌───────────────────────┐        ┌──────────────────────┐   │
   │   │   Desktop Agent (Go)  │  stdio │   Claude Code        │   │
   │   │                       │◀──────▶│  (user's own tool)   │   │
   │   │  • adapter registry   │        └──────────────────────┘   │
   │   │  • session manager    │                                   │
   │   │  • power manager      │        ┌──────────────────────┐   │
   │   │  • encrypted store    │───────▶│  OS power API        │   │
   │   │  • WS transport       │        └──────────────────────┘   │
   │   └───────────┬───────────┘                                   │
   └───────────────┼───────────────────────────────────────────────┘
                   │  WSS, versioned protocol, 30s heartbeat
                   ▼
   ┌─────────────────────────── Railway ───────────────────────────┐
   │   ┌───────────────────────────────────────────────────────┐   │
   │   │              Backend (Go + Fiber)                     │   │
   │   │                                                       │   │
   │   │   adapter/http    adapter/ws    adapter/oauth         │   │
   │   │        └──────────────┴──────────────┘                │   │
   │   │                   app (use cases)                     │   │
   │   │                        │                              │   │
   │   │                  port (interfaces)                    │   │
   │   │                        │                              │   │
   │   │                     domain                            │   │
   │   │        ┌──────────────┴──────────────┐                │   │
   │   │   adapter/postgres            adapter/redis           │   │
   │   └────────────┬──────────────────────┬───────────────────┘   │
   └────────────────┼──────────────────────┼───────────────────────┘
                    ▼                      ▼
         ┌──────────────────┐   ┌──────────────────┐
         │ Supabase         │   │ Upstash Redis    │
         │ PostgreSQL       │   │ (ephemeral only) │
         │ system of record │   └──────────────────┘
         └──────────────────┘
                    ▲
                    │ WSS + REST
         ┌──────────┴──────────┐
         │ Dashboard (Next.js) │  ← phone or laptop
         │ hosted on Vercel    │
         └─────────────────────┘
```

---

## 3. Clean Architecture in the backend

Four layers, with dependencies pointing strictly inward:

```
adapter  ───▶  port  ◀───  app  ───▶  domain
(drivers)   (interfaces) (use cases)  (rules)
```

| Layer | May import | Contains |
| --- | --- | --- |
| `domain` | stdlib, `shared/protocol` | Entities, invariants, domain errors |
| `port` | `domain` | Interfaces the use cases depend on |
| `app` | `domain`, `port` | Use cases, orchestration, authorisation |
| `adapter` | `domain`, `port`, `app` | Fiber, pgx, Redis, OAuth, WebSocket |

### Why this is worth the indirection

The abstraction earns its keep against a specific, likely event. PROJECT.md names
Supabase and Upstash — both managed services that a team might outgrow, and that a
self-hoster will certainly want to replace. Because no business rule imports a
driver, replacing either touches only `internal/adapter`. Had a session rule
imported `pgx`, that swap would be a rewrite instead of a new file.

The second payoff is test cost. A use case depends on interfaces declared in
`port`, so its tests substitute in-memory fakes and need no database, no container,
and no network. Cheap tests get written; expensive ones get skipped.

### Dependency inversion, concretely

Interfaces live in `port`, next to their **consumer**, not next to their
implementation:

```go
// internal/port/prompt.go — owned by the app layer
type PromptQueueStore interface {
    Enqueue(ctx context.Context, p domain.QueuedPrompt) error
    NextForDevice(ctx context.Context, deviceID string) (domain.QueuedPrompt, error)
}

// internal/adapter/postgres/prompt.go — satisfies it implicitly
type PromptStore struct{ db *pgxpool.Pool }
```

Go's implicit interface satisfaction is what makes this cheap: the Postgres adapter
never imports `port`, it simply has the right method set. The dependency arrow
points inward with no ceremony.

### Interface granularity

Interfaces are narrow and named for the caller's need. One `Repository` interface
with thirty methods would force every fake to implement thirty methods to test one
— which is how tests get expensive enough that people stop writing them. Prefer
`SessionReader` and `SessionWriter` over `SessionStore`.

---

## 4. The `shared` module and its zero-dependency rule

`shared/` is imported by **both** binaries and has **no third-party dependencies**
at all. CI fails the build if a `require` appears in `shared/go.mod`
([ADR-0003](adr/0003-shared-module-is-protocol-only.md)).

The reason is contamination. Anything added to `shared` lands in both the backend
and the agent. The agent is a small binary distributed to users' machines and must
not carry Fiber; the backend must not carry Windows power syscalls. A dependency-free
`shared` makes that violation impossible rather than merely discouraged.

| Package | Purpose | Notable design point |
| --- | --- | --- |
| `protocol` | The wire format: envelope, 13 message types, payloads, state machine | Both peers compile against the same types, so producer/consumer drift is a build error rather than a production bug |
| `config` | The CLI → env → file → defaults precedence engine | Takes the file decoder as an injected function, so YAML stays out of `shared` ([ADR-0004](adr/0004-configuration-precedence-engine.md)) |
| `log` | `log/slog` JSON logging with correlation fields | Redacts secrets in the handler, so a careless call site cannot leak a token |
| `id` | ULID-compatible sortable identifiers | Time-prefixed for PostgreSQL index locality ([ADR-0005](adr/0005-ulid-identifiers.md)) |
| `retry` | Exponential backoff with jitter | One implementation for reconnect, HTTP retry, and prompt redelivery |
| `lifecycle` | Ordered startup, reverse-order shutdown | Reverse ordering is what stops a deploy dropping in-flight requests |
| `version` | Build metadata via `-ldflags` | An agent on a machine we cannot inspect can state its own provenance |

### Why the protocol is shared code rather than a schema

The alternative was a schema file (Protobuf, JSON Schema) with generated types. A
shared Go package was chosen because both peers are Go: the types are the schema,
a payload change breaks compilation on both sides immediately, and there is no code
generation step in the build. The cost is that a non-Go client would need the
protocol reimplemented from [WEBSOCKET_PROTOCOL.md](WEBSOCKET_PROTOCOL.md) — an
acceptable trade while both peers are ours, and revisitable if the planned public
SDK arrives.

---

## 5. The Desktop Agent

The agent is the part that makes Beuvian work, and the part with the least control
over its environment. It runs on a laptop that sleeps, changes networks, and gets
closed mid-task.

### The adapter abstraction

Everything about "which coding agent" is confined behind one interface
([`agent/internal/coding`](../agent/internal/coding/adapter.go)):

```go
type Adapter interface {
    Name() string
    Start(ctx context.Context, opts StartOptions) error
    Stop(ctx context.Context) error
    Status() Status
    SendPrompt(ctx context.Context, prompt string) error
    ReadOutput() <-chan OutputLine
    CurrentTask() string
    Repository() string
    WorkingDirectory() string
    ExitCode() (int, bool)
}
```

Two deliberate departures from the method list in PROJECT.md:

- **I/O methods take a `context.Context`.** A `Stop()` with no deadline is the
  difference between a clean exit and a wedged process left on a user's machine.
- **`ReadOutput` returns a channel.** Output is an unbounded stream at
  unpredictable rates; a pull-based reader would either block the caller or drop
  lines silently.

`Detector` is a **separate** interface from `Adapter`. That separation is what lets
Beuvian truthfully report "Claude Code is installed at `C:\...\claude.cmd`" in
Phase 1, before any adapter can drive it — and it is why `-detect` works today.

Adapters are constructed through a `Registry` rather than a `switch` in the session
manager, so adding Codex CLI requires no edit to session code. The registry is an
instance, not a package global: a global would be mutable shared state and would
let tests leak registrations into each other.

### Session state machine

```
        ┌──────────────────────────────────────┐
        ▼                                      │
      idle ──▶ starting ──▶ running ──▶ stopping ──▶ stopped ──┐
                  │            ▲  │                    ▲       │
                  │            │  ▼                    │       │
                  │      waiting_input ────────────────┘       │
                  │            │                               │
                  └────────────┴──▶ crashed ────────────────────┘
                                       │                       │
                                       └──▶ starting ◀─────────┘
```

Modelled as one enum with a declared transition table rather than a set of booleans
(`isRunning`, `isWaiting`, `hasCrashed`). Booleans permit contradictory
combinations that then have to be defended against at every read site; one enum
makes an invalid state unrepresentable and lets the dashboard render from a single
field.

`AgentState.Active()` is the predicate that holds the sleep inhibition. If a
terminal state ever reported active, a finished session would pin the user's
machine awake indefinitely — so it is asserted in tests directly.

### Waiting-for-input detection is a heuristic

Claude Code emits no machine-readable "I need you" signal. Idleness is inferred
from output falling silent for `session.idle_timeout` (default 20s).

The two failure modes are **not** symmetric. A premature notification is mildly
annoying. A missed one means the user believes work is progressing when it stopped
forty minutes ago — the exact failure Beuvian exists to prevent. The detector
therefore leans toward notifying, and the dashboard shows state clearly enough that
a false positive is obvious at a glance.

### Power management

`PreventSleep` / `AllowSleep` / `Status` behind one interface, with per-OS
build-tagged implementations so no `runtime.GOOS` branching appears in logic and
each target compiles only code that can run on it. CI cross-compiles and vets all
six OS/arch combinations, because a Windows-only syntax error in
`power_windows.go` would otherwise reach a release.

Phase 1 ships the build structure and the honest `unsupportedManager`; Phase 3
implements the syscalls. The planned approaches, and the traps in each, are recorded
in the platform files themselves — for example, that `SetThreadExecutionState` is
thread-affine and requires `runtime.LockOSThread`, which is the most common way that
API is misused.

---

## 6. Realtime transport

Full details in [WEBSOCKET_PROTOCOL.md](WEBSOCKET_PROTOCOL.md). The architectural
points:

**One socket per connection, versioned envelope.** The backend accepts versions in
`[MinSupportedVersion, Version]`, so an older agent keeps working after a backend
deploy. Agents live on users' machines and cannot be upgraded in lockstep — this is
a hard requirement, not a nicety.

**Reconnection is the normal case.** The agent's reconnect loop is unbounded on
purpose: a laptop closed for the weekend must reconnect on Monday, not have given
up on Friday. The one thing that must never retry forever is a rejected credential
— an agent with a revoked token retrying every 30 seconds becomes a
denial-of-service against our own gateway, multiplied by every installed agent.
`ErrorCode.Retryable()` draws that line and **defaults to non-retryable**, so a
carelessly added code fails safe.

**Jitter is load-bearing.** When the backend restarts, every agent reconnects at
once. Without jitter their retries stay synchronised and the herd keeps the gateway
down — the outage becomes self-sustaining. `ReconnectPolicy` uses 30% jitter for
this reason.

**Slow consumers are dropped, not tolerated.** Each connection has a bounded
outbound queue (`websocket.send_queue_size`). When it fills, that connection is
closed rather than blocking the broadcaster, because one stalled phone on a train
must not freeze every other client.

---

## 7. Data: what goes where

### PostgreSQL is the system of record

All durable business data. See [DATABASE.md](DATABASE.md).

### Redis is strictly ephemeral

PROJECT.md is explicit: *"Never store permanent business data"* in Redis. Redis
holds presence, heartbeat, the hot prompt-dispatch queue, rate-limit counters,
distributed locks, and short-lived cache.

**The one tension worth naming:** PROJECT.md lists a `prompt_queue` table *and*
"Prompt Queue" under Redis. These are not duplicates and neither is wrong. The
resolution: **PostgreSQL is authoritative; Redis is a dispatch accelerator.** A
prompt is written to `prompt_queue` and committed before the API acknowledges the
user, then published to Redis for immediate delivery. If Redis loses it, the
prompt is still delivered from PostgreSQL on the next reconnect or poll. This is
why `redis.required` defaults to `false` — losing hot dispatch degrades latency,
while losing a user's instruction would be data loss.

The compose file sets `maxmemory-policy noeviction` for the same reason: silently
evicting a queued prompt under memory pressure would lose a user's instruction, so
a hard failure is the correct behaviour for a queue.

---

## 8. Configuration and startup

`CLI > Environment > Config file > Defaults`, implemented once in `shared/config`
and used by both binaries ([ADR-0004](adr/0004-configuration-precedence-engine.md)).

Two details that are easy to get wrong and are covered by tests:

- **Only flags the user actually typed are applied.** The engine uses
  `FlagSet.Visit`, not `VisitAll`. With `VisitAll`, every registered-but-untyped
  flag would overwrite the environment and the file with its empty default.
- **Two passes are unavoidable.** The config file's *location* is itself set by flag
  and env, so flags must be parsed before the file is read but applied after it.

**Validation is aggregated and environment-aware.** Every problem is reported
together, because a fresh deployment typically has several settings missing at once
and one-at-a-time errors turn that into a guessing game. Production additionally
rejects plaintext origins, insecure cookies, signing secrets under 32 bytes, and
debug logging. Development tolerates a missing database and Redis so a clean clone
runs with zero setup — and warns loudly, so the eventual production failure is not
a surprise.

### Lifecycle

Both binaries assemble components into a `lifecycle.Supervisor`: startup in
registration order, shutdown in **reverse**. Reverse ordering is the property that
matters — the HTTP server must stop accepting requests before the database pool it
depends on closes, or in-flight requests fail on every deploy.

The shutdown context is derived with `context.WithoutCancel`. Deriving from the
already-cancelled parent would hand every `Stop` an expired deadline and skip the
graceful path entirely — the exact bug graceful shutdown exists to prevent. There is
a regression test for it.

---

## 9. Security posture

| Concern | Approach |
| --- | --- |
| Transport | HTTPS and WSS only; production config rejects plaintext origins |
| User sessions | Short-lived JWT access tokens (15m) + rotating refresh tokens stored hashed |
| Device identity | Per-device tokens, separate from dashboard sessions — a leaked device token must not grant dashboard access |
| Replay | AUTH carries a single-use nonce; envelopes carry a timestamp bounded by `MaxClockSkew` (2m). The window limits exposure; the nonce cache defeats replay inside it |
| Local secrets | Agent state encrypted at rest, keyed via OS secret store (DPAPI / Keychain / Secret Service) |
| Log leakage | `shared/log` redacts secret-shaped keys in the handler — policy, not convention |
| Rate limiting | Redis-backed, per identity; a tighter budget on auth endpoints |
| Container | Distroless, non-root, no shell — CI fails the build if the image runs as root |
| Dependencies | `govulncheck` per module in CI; `shared` has no dependencies to scan |

### An honest limitation

The agent's encrypted local state is protected by the OS secret store, which binds
the key to the OS user. Because the agent must start unattended, with no one present
to type a passphrase, the key is by necessity recoverable by the process itself.
Against a determined attacker who already has code execution as that user, this is
obfuscation plus OS protection rather than true secrecy. It is documented as such in
[`agent/internal/store`](../agent/internal/store/doc.go) instead of being presented
as stronger than it is.

---

## 10. Extension points

PROJECT.md requires the future integrations to be *designed* and not built. Each is
an interface with exactly one implementation today:

| Future feature | Extension point |
| --- | --- |
| WhatsApp, Telegram, Slack, Discord, push, voice | `port.NotificationChannel` — the MVP has only the dashboard channel |
| Codex, Gemini, Aider, OpenHands | `coding.Adapter` + `Registry` — already registered as detecting placeholders |
| Billing, subscriptions | `port.BillingProvider` |
| Organisations, teams | `port.OrganizationStore`; tables carry a nullable `organization_id` from the start, so adding teams is not a migration of every row |
| Cloud-hosted agents | The same `Adapter` interface with a remote transport |
| Public REST API, SDK | Use cases already have no transport dependency, so a second entry point adds no business logic |

The test of whether an extension point is real: a placeholder adapter has to satisfy
the full `Adapter` interface. If it could not, the abstraction would be
Claude-shaped and the pluggability a fiction. `agent/internal/coding` has a
compile-time assertion and tests for exactly this.

---

## 11. What Phase 1 does and does not include

**Included and verified:** the monorepo and workspace, all three modules building
independently, the complete wire protocol with tests, the configuration engine with
precedence tests, structured logging with redaction tests, the lifecycle supervisor
with ordering tests, the adapter registry with placeholders and real detection,
the cross-platform power scaffolding, Docker, CI/CD, and this documentation.

**Not included:** any HTTP server, the database schema, Redis wiring, the real
Claude adapter, the power syscalls, and the dashboard. Those are Phases 2–4, and the
bootstrap in both `main.go` files is written so they arrive as additional
`sup.Add(...)` calls rather than as a restructuring.
