# ADR-0007: A registry-based pluggable coding-agent adapter

**Status:** Accepted · Phase 1

## Context

PROJECT.md requires Claude Code support in the MVP, placeholder adapters for Codex CLI,
Gemini CLI, Aider, and OpenHands, and that **"future adapters must require minimal code
changes."** It specifies the interface method set:

```
Start() Stop() Status() SendPrompt() ReadOutput()
CurrentTask() Repository() WorkingDirectory() ExitCode()
```

The risk with an abstraction designed against exactly one implementation is that it
ends up shaped like that implementation. An interface written solely around Claude
Code's behaviour would appear pluggable while being unusable for anything else — and
the problem would only surface when the second adapter was attempted, by which point
every caller depends on the wrong shape.

## Options considered

**1. Claude-specific code, abstract later.** Fastest to the MVP, and the standard
advice against premature abstraction. But PROJECT.md names the extension point as a
requirement, and retrofitting one after the session manager, transport, and status
reporting all reference Claude concretely means touching all of them.

**2. An interface plus a `switch` in the session manager.** An interface for testability
with construction by `switch adapterName`. Simple, and the abstraction is real. The flaw
is that adding Codex means editing session code — which is precisely the "minimal code
changes" requirement failing.

**3. An interface plus a registry.** Adapters register a factory under a name; the
session manager resolves by name and never learns which concrete type it got.

**4. Interface, registry, and a plugin system** (Go plugins, or subprocesses speaking a
protocol). Maximum extensibility, and it would serve the "plugin marketplace" listed
under future extensions. Vastly more machinery — process supervision, versioning, a
second protocol — for a need that does not exist yet.

## Decision

Option 3.

```go
type Factory func(logger *slog.Logger) Adapter

type Registry struct { /* name → Factory, name → Detector */ }
```

Adding an adapter is one file plus one `Register` call. Nothing in `session/`,
`transport/`, or `main.go` changes — and if it does, the abstraction has leaked and
that leak is the bug.

### Three deliberate departures from the specified interface

**I/O methods take a `context.Context`.** PROJECT.md lists `Stop()`. A `Stop` with no
deadline is the difference between a clean exit and a wedged coding-agent process left
running on a user's machine after Beuvian has gone. The context is what lets Stop
attempt a graceful exit and escalate to a kill when the budget expires.

**`ReadOutput` returns a channel, not bytes.** Output is an unbounded stream arriving at
unpredictable rates — a verbose build emits thousands of lines per second. A
`ReadOutput() []byte` would either block the caller or drop lines silently. The channel
closes when the process exits, which is how the session manager learns the stream ended
without polling.

**`ExitCode` returns `(int, bool)`.** The bool is false while the process runs. A bare
`int` cannot distinguish "exited with 0" from "has not exited", and conflating those
would make a running session look successfully finished.

### `Detector` is a separate interface

```go
type Detector interface {
    Name() string
    Detect(ctx context.Context) (Installation, error)
}
```

Detection happens before any process exists, and the dashboard needs to show what is
available on a device without starting anything. Splitting it has an immediate payoff:
**Phase 1 can truthfully report "Claude Code is installed at `C:\...\claude.cmd`"
before any adapter can drive it.** `beuvian-agent -detect` works today because of this
split. Conflating them would have forced a choice between lying about what is installed
and hiding installations already visible.

### The registry is an instance, not a package global

A package-level registry with `init()` registration is the common Go pattern. It is
rejected here for two reasons: PROJECT.md prohibits global mutable state, and a global
lets test registrations leak into each other, which produces order-dependent failures
that are miserable to diagnose.

### Placeholders satisfy the full interface

The four future adapters are registered with factories whose methods return
`ErrNotImplemented` — never a zero value. A placeholder that returned `nil` from
`Start` would present as a coding agent that accepted the work and then did nothing,
which is the worst available behaviour: the user waits for a result that will never
arrive.

**The placeholders are the test of whether the abstraction is real.** A placeholder has
to satisfy every method. If it could not — if some method only made sense for Claude —
the interface would be Claude-shaped and the extension point a fiction. There is a
compile-time assertion and a test using a non-Claude stub adapter for exactly this
purpose.

`Implemented(name)` is the single place Phase 3 edits when the real Claude adapter
lands, and `TestImplementedIsHonestAboutPhase1` fails at that moment so the change is
deliberate rather than incidental.

### Capabilities report reality

`Registry.Capabilities()` returns adapters **installed on this machine**, not those
compiled in. A binary supporting five adapters on a machine with one installed reports
one capability. The backend uses this to avoid dispatching a prompt to a device that
cannot service it — the alternative is a prompt that fails on the device, after the user
has walked away.

## Consequences

**Gained**

- Adding a coding agent is one file and one registration.
- The abstraction is validated by four placeholders and a non-Claude test stub, so it
  cannot silently be Claude-shaped.
- `-detect` is useful in Phase 1, before any adapter works.
- Sessions cannot be started against an uninstalled adapter.
- No global state, so tests are independent.
- The same interface serves the "cloud-hosted agents" extension with a remote transport.

**Accepted costs**

- **Indirection with one real implementation.** For the MVP this is more structure than
  strictly needed, and that is a fair criticism of any abstraction built to a
  requirement rather than to a second use case.
- **Registration is a step that can be forgotten.** A new adapter that is implemented
  but not registered is silently unavailable. Mitigated by a test asserting all five
  spec-named adapters are registered.
- **The interface may still prove wrong.** Placeholders prove it is *satisfiable*, not
  that it is *sufficient*. A real Codex adapter may need something absent — session
  resumption, a structured event stream instead of lines. That would be an interface
  change touching every implementation, and it is the genuine residual risk here.
- **`ReadOutput` implies a single consumer.** The session manager owns the channel and
  fans out from there. Not stated in the type system, so it is documented instead.

## Revisit if

- A second real adapter needs a method the interface lacks. Add it deliberately, update
  every implementation, and amend this record.
- The plugin marketplace in PROJECT.md's future extensions becomes real, at which point
  option 4 becomes the question — and the registry is the seam it would be built on.
