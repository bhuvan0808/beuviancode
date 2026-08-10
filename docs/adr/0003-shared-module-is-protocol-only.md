# ADR-0003: The shared module has zero third-party dependencies

**Status:** Accepted · Phase 1

## Context

`shared/` is imported by **both** the backend and the Desktop Agent. Anything added
to it lands in both binaries.

Those two binaries have opposite constraints. The backend runs in a container we
control, where a dependency costs almost nothing. The agent is distributed to users'
machines, cross-compiled for six OS/arch combinations, must build without cgo to stay
cross-compilable, and cannot be upgraded on demand once installed. Every dependency it
carries is a supply-chain risk on someone else's laptop and a potential obstacle to
cross-compilation.

A "shared" package is also where architecture decays quietest. The natural pull is
toward putting anything two components need there, and a shared module with fifteen
dependencies is no longer a contract — it is a second application that both binaries
must carry.

## Options considered

**1. No constraint.** Add dependencies as needed. Convenient, and each individual
addition is defensible. The failure is cumulative: nobody makes the decision to give
the agent a heavy dependency tree, it simply arrives one reasonable commit at a time.

**2. A documented guideline.** "Keep shared light." Costs nothing and achieves
nothing — a guideline with no enforcement loses to a deadline every time.

**3. A hard zero-dependency rule, enforced in CI.** `shared/go.mod` must contain no
`require` block. Absolute and unambiguous, but it forces awkwardness whenever shared
code genuinely wants a library.

**4. An allowlist.** Permit specific vetted dependencies. More flexible, but it turns
every addition into a negotiation and the list itself into a thing to maintain.

## Decision

Option 3. `shared/go.mod` has no `require` block, and both `make verify-shared-deps`
and a dedicated CI job fail the build if one appears.

The rule is stated at the top of `shared/go.mod` itself, so anyone about to add a
dependency reads the reason before doing it.

### Making it work without libraries

The rule is only tenable because the shared code's needs are genuinely met by the
standard library, and where they are not, the dependency can be inverted:

| Need | Stdlib solution |
| --- | --- |
| Structured JSON logging | `log/slog` |
| Wire protocol | `encoding/json` + generics for typed payload decoding |
| Sortable IDs | `crypto/rand` + a 40-line Crockford base32 encoder |
| Backoff jitter | `crypto/rand`, no PRNG library |
| Lifecycle supervision | `os/signal`, `context` |
| Build metadata | `runtime/debug` |

The interesting case is configuration. `shared/config` must apply CLI, env, file, and
default layers — and parsing YAML needs a third-party library. Rather than either
breaking the rule or duplicating the precedence logic in both binaries, the package
takes the decoder as an injected function:

```go
type DecodeFunc func(data []byte, dst any) error

// The caller supplies yaml.Unmarshal; shared/config never imports YAML.
config.Resolve(cfg, config.Options{Decode: yaml.Unmarshal, ...})
```

The signature deliberately matches `yaml.Unmarshal` and `json.Unmarshal`, so callers
pass the library function directly with no adapter. The *policy* (what beats what) is
centralised; the *format* dependency stays in the binaries that need it. Its own tests
use `encoding/json` as the decoder, so the test suite honours the invariant too.

That inversion is the general answer whenever this rule bites: take what you need as a
parameter.

## Consequences

**Gained**

- The agent binary carries only the standard library plus `yaml.v3`. Roughly 3 MB,
  cross-compiled for six targets with no cgo.
- `govulncheck` has nothing to report for `shared` because there is no third-party
  code in it.
- No dependency in `shared` can obstruct cross-compilation — a single cgo-requiring
  library would end the six-target build.
- The constraint forces the inversion in option-4 situations, which produces better
  design than the library would have. `shared/config` works for TOML and JSON without
  modification precisely because it does not know about YAML.
- "Is this shared or does it belong to one side?" has a sharp answer instead of a
  judgement call.

**Accepted costs**

- Hand-written code where a library would do. The ULID encoder is the clearest case:
  about 40 lines that `oklog/ulid` provides for free, and it needed its own tests to
  be trustworthy.
- Real friction when shared code wants a library. Sometimes the inversion is elegant,
  as with the config decoder; sometimes it will just be inconvenient.
- A hard rule cannot accommodate a genuinely good exception without being amended.
  That is the point, but it means the amendment has to be a deliberate act — which is
  what an ADR is for.

## Revisit if

- Two or more inversions become contorted enough that they are harder to understand
  than the dependency would have been. That is the signal the rule has stopped paying
  for itself, and the replacement should be option 4 with an explicit allowlist rather
  than option 1.
