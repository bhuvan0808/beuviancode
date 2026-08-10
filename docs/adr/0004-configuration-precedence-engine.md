# ADR-0004: One reflection-based configuration precedence engine

**Status:** Accepted · Phase 1

## Context

PROJECT.md requires `config.yaml`, environment variables, and CLI flags, resolved in
the order **CLI → Environment → Config file → Defaults**, for *both* binaries. It also
requires "no duplicated business logic".

Two independent loaders would be duplicated logic in the most damaging place: silently
divergent precedence. A bug where the backend honours a flag over the environment and
the agent does not would surface as "this setting doesn't work" months later, and
nobody would think to compare the two loaders.

The complication is that the precedence engine belongs in `shared/`, which
[ADR-0003](0003-shared-module-is-protocol-only.md) forbids from having dependencies —
and parsing YAML requires one.

## Options considered

**1. Viper.** The default answer for Go configuration. Handles all four layers, plus
watching and remote sources. But it pulls a large dependency tree into `shared` and
therefore into the agent, violating ADR-0003. Its precedence order is also its own
rather than ours, and PROJECT.md specifies exactly what ours must be.

**2. `kelseyhightower/envconfig` plus hand-rolled flag and file handling.** Lighter,
but still a dependency in `shared`, and it covers only the environment layer — the
flag/file/precedence logic would still need writing.

**3. A loader in each binary.** No shared dependency problem at all. But it is the
duplication PROJECT.md prohibits, with the divergence risk described above.

**4. A precedence engine in `shared` with the file decoder injected.** Reflection over
struct tags for defaults, environment, and flags; the file layer delegated to a
caller-supplied function.

## Decision

Option 4. `shared/config` owns the precedence policy using only the standard library;
each binary supplies `yaml.Unmarshal` for the file layer.

```go
type DecodeFunc func(data []byte, dst any) error   // matches yaml.Unmarshal exactly
```

The configuration struct is the schema, declared once with its tags:

```go
Port int `yaml:"port" env:"PORT" flag:"port" default:"8080" usage:"listen port"`
```

Tags: `env`, `flag`, `default`, `usage`, `secret`. Nested structs compose their `env`
prefixes, so a `Server` section tagged `env:"SERVER"` yields `BEUVIAN_SERVER_PORT`.

### Two implementation details that are load-bearing

**Only flags the user actually typed are applied.** The engine uses
`flag.FlagSet.Visit`, not `VisitAll`. `Visit` iterates only flags present on the
command line. With `VisitAll`, every registered-but-untyped flag would apply its
default and overwrite the environment and config file — meaning simply *registering* a
flag would break the precedence order. Flags are also registered as strings regardless
of destination type, because a typed flag holding its default is indistinguishable from
one the user set to that same value.

`TestUnsetFlagDoesNotOverride` exists specifically for this.

**Resolution requires two passes over the command line.** The config file's *location*
is itself configurable by flag and environment, so flags must be parsed **before** the
file can be read, yet applied **after** it to retain top priority. Pass one parses and
records; pass two replays only what the user typed.

### Related decisions

- `LookupEnv`, not `Getenv`: an explicitly exported empty value (`BEUVIAN_PROXY=""`) is
  an intentional override, not "unset".
- Validation is **aggregated** with `errors.Join`. A fresh deployment usually has
  several settings missing at once, and one-error-per-run turns that into a guessing
  game.
- `Describe()` renders the effective configuration with `secret` fields redacted, for a
  single startup log line. It distinguishes `<redacted>` from `<unset>` because a
  missing secret is usually the actual bug.
- The engine returns which file was loaded. "Why is this setting not what I set?" is
  most often answered by "a different file than you think was read", so startup logs it.

## Consequences

**Gained**

- One precedence implementation, so the two binaries cannot diverge.
- ADR-0003 holds; YAML stays out of `shared`.
- Adding a setting is one tagged struct field — no registration, no parsing, no
  plumbing.
- The engine works unchanged for TOML or JSON, and its own tests use `encoding/json` as
  the decoder, which means the test suite honours the zero-dependency invariant too.
- Precedence is covered by tests where all four layers set the same field, so only the
  correct winner passes.

**Accepted costs**

- Reflection means configuration errors are runtime rather than compile-time. A typo in
  an `env` tag is not caught by the compiler. Mitigated by `Describe()` making the
  effective values visible and by tests over the real config structs.
- Only a fixed set of field types is supported: string, bool, the integer and float
  kinds, `time.Duration`, and `[]string`. Anything else fails with "unsupported config
  field type". That covers everything the two binaries need, and the failure is loud
  rather than silent.
- `[]string` parses as comma-separated, which cannot express a value containing a
  comma. Chosen because it is what shells and the Railway and Vercel environment editors
  handle cleanly; CORS origin lists are the main consumer and never contain commas.
- Reflection is slower than generated code. Irrelevant: this runs once at startup.
- The two-pass flag handling is genuinely subtle. It is documented in the code and in
  this record, because someone will otherwise "simplify" it into a bug.

## Revisit if

- A configuration source is needed that this shape cannot express — a remote provider,
  or live reloading. Both are outside PROJECT.md's requirements today.
- Struct types beyond the supported set are needed often enough that the reflection
  switch becomes a maintenance burden.
