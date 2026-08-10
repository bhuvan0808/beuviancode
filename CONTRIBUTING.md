# Contributing to Beuvian

Thanks for your interest. Beuvian is MIT-licensed and contributions are welcome.

## Before you start

Read [PROJECT.md](PROJECT.md). It is the product specification and the source of
truth. If a change conflicts with it, say so in the issue or pull request rather
than quietly diverging — the spec can be wrong, but that should be an explicit
decision.

Then read the `doc.go` of whatever package you are changing. Each one states its
layer contract, and most of the "why is it like this" questions are answered there.

## Getting set up

```bash
git clone https://github.com/bhuvan0808/beuviancode.git
cd beuvian
make check      # build + vet + test + format, every module
```

That must pass on a clean clone with no configuration. If it does not, that is a bug
worth reporting on its own.

[DEVELOPER_GUIDE.md](docs/DEVELOPER_GUIDE.md) has the details.

## Workflow

1. Open an issue first for anything non-trivial. It is cheaper to disagree about an
   approach in an issue than in a finished pull request.
2. Branch from `main`: `feat/…`, `fix/…`, `docs/…`, `refactor/…`, `test/…`.
3. Make the change, with tests.
4. Run `make check`.
5. Open a pull request describing **what** changed and **why**.

## What CI enforces

Everything below runs on every pull request, per module:

| Check | Command |
| --- | --- |
| Formatting | `gofmt -l .` must be empty |
| `go.mod` tidy | `go mod tidy` must produce no diff |
| Independent compilation | `GOWORK=off go build ./...` |
| Vet | `go vet ./...` |
| Tests with race detector | `go test -race ./...` |
| Lint | `golangci-lint` |
| Vulnerabilities | `govulncheck ./...` |
| `shared` has no dependencies | no `require` in `shared/go.mod` |
| Cross-compilation | agent builds for 6 OS/arch combinations |
| Container | image builds, runs, and is not root |

`make check` covers the common ones locally.

## Standards

These come from PROJECT.md and are applied consistently. Please match them.

### Comments explain *why*

The code already says what it does. A comment that restates the next line is noise;
a comment that explains a non-obvious choice is the most valuable thing in the file.

```go
// Poor — restates the code.
// Set the timeout to 75 seconds.
HeartbeatTimeout = 75 * time.Second

// Useful — explains the choice and what breaks without it.
// HeartbeatTimeout is 2.5x the interval so a single dropped heartbeat does not
// tear down an otherwise healthy connection.
HeartbeatTimeout = 75 * time.Second
```

If you pick a number — a timeout, a buffer size, a default — say what goes wrong if
it changes.

### Errors

- Wrap with `%w`; compare with `errors.Is` / `errors.As`, never by string.
- Sentinel errors are part of a package's contract. Document them.
- Aggregate validation errors with `errors.Join` rather than returning the first.
- Error messages should name the offending thing and be actionable.

### Architecture

- **Respect the layers.** `adapter → port ← app → domain`. A use case that imports a
  driver cannot be tested without that driver running, which is the whole cost this
  boundary avoids.
- **Interfaces live with their consumer**, not their implementation.
- **Keep interfaces narrow.** A thirty-method interface forces every fake to
  implement thirty methods to test one, which is how tests get expensive enough that
  people stop writing them.
- **No package-level mutable state.** Dependencies come through constructors.
- **Contexts on anything doing I/O**, and honour cancellation.
- **Never `os.Exit` outside `main`** — it skips deferred cleanup.

### Tests

Assert behaviour that matters, and say why in a comment. Several existing tests are
deliberate **tripwires** that fail when an assumption changes, forcing a decision
instead of letting it drift:

- `TestMessageTypeSetMatchesSpec` — fails if the protocol gains a type without the
  spec being updated.
- `TestImplementedIsHonestAboutPhase1` — fails when the first real adapter lands.
- `TestHeartbeatTimeoutToleratesOneMissedBeat` — fails if the timeout is tightened
  below 2× the interval.

If you change one of these, change it deliberately and say so in the pull request.
That is the point of them.

Use `t.Setenv` rather than `os.Setenv`, and `t.TempDir()` for files. Prefer
table-driven tests.

### Changing the protocol

`shared/protocol` is a contract compiled into both binaries and into agents already
installed on users' machines.

1. Decide whether the change is additive. Adding an optional field or a new message
   type does **not** bump `Version`. Removing a field, renaming one, or changing its
   meaning **does**.
2. Update [WEBSOCKET_PROTOCOL.md](docs/WEBSOCKET_PROTOCOL.md) **in the same commit**.
3. Keep `MinSupportedVersion` covering the oldest agent you intend to support. Agents
   cannot be upgraded in lockstep with the server — that is a hard constraint, not a
   preference.

### Adding a coding agent adapter

The extension point exists for this and should require no changes outside
`agent/internal/coding/`:

1. Implement `coding.Adapter`.
2. Add executable names to `knownAdapters()`.
3. Add the name to `Implemented()`.
4. Replace the placeholder factory in `RegisterPlaceholders`.

If you find yourself editing `session/`, `transport/`, or `main.go`, the abstraction
has leaked — that leak is the bug to fix, and it is worth raising rather than working
around.

### Architecture Decision Records

If your change makes a decision with real alternatives, add an ADR in
[`docs/adr/`](docs/adr/). Copy the shape of an existing one: context, the options
considered, the decision, and the consequences **including the downsides you
accepted**. An ADR that lists no drawbacks is not describing a real decision.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/):

```
feat(agent): implement Claude Code adapter
fix(backend): reset backoff after a successful reconnect
docs(protocol): document the ACK-for-AUTH convention
refactor(shared): extract the precedence engine
test(config): cover the unset-flag override case
```

Explain *why* in the body when it is not obvious from the subject.

## Security

Please do **not** open a public issue for a vulnerability. Report it privately so a
fix can ship before the details are public.

Areas where care is especially warranted:

- Token issuance, validation, and refresh rotation
- The WebSocket authentication handshake and replay protection
- Agent local state encryption and key handling
- Anything that logs — `shared/log` redacts secret-shaped keys as a backstop, but do
  not rely on it

## Scope

Beuvian **never** calls a model API and never handles provider credentials. A pull
request that adds an Anthropic or OpenAI dependency will be declined regardless of
quality: it changes what the product is, not just how it works.

Features listed in PROJECT.md under future extensions (WhatsApp, Telegram, billing,
organisations) should arrive as implementations of the existing interfaces rather
than as new subsystems. If the interface does not fit, that is worth discussing
first.

## Code of conduct

Be decent to people. Assume good faith, disagree about the work rather than the
person, and take the time to explain your reasoning — most disagreements here are
about tradeoffs, and tradeoffs are worth arguing carefully.
