# ADR-0002: Three Go modules with a workspace and replace directives

**Status:** Accepted · Phase 1

## Context

PROJECT.md specifies a monorepo containing `agent/`, `backend/`, `dashboard/`, and
`shared/`, and states: **"Every module must compile independently."**

The Go modules have genuinely divergent dependency needs. The backend wants Fiber, a
PostgreSQL driver, and a Redis client. The agent wants OS power syscalls and process
supervision. Neither wants the other's tree — and the agent especially, since it is a
binary distributed to users' machines where every megabyte and every dependency is a
liability.

## Options considered

**1. One `go.mod` at the repository root.** Simplest possible setup: one dependency
graph, one `go.sum`, one `go mod tidy`. But it fuses the dependency graphs. The agent
binary's module would require Fiber, and although the linker drops unused code, `go
mod tidy` cannot distinguish "the agent needs this" from "something in the repo needs
this". It also fails PROJECT.md's independent-compilation requirement outright: there
would be one module, not several.

**2. Separate modules with `replace` directives only.** Each module declares
`replace github.com/bhuvan0808/beuviancode/shared => ../shared`. This works everywhere,
including Docker and a single-module clone. But before `go.work` existed this was the
only option, and it has a known failure: a `replace` directive is baked into the
published module, so `go install` of a tagged version resolves to a path that does not
exist on the installing machine.

**3. Separate modules with `go.work` only.** The modern answer. The workspace resolves
cross-module references for local development with no `replace` in any `go.mod`. But
`go.work` is not present in every build context: the Docker image copies only
`shared/` and `backend/`, and CI deliberately runs with `GOWORK=off` to prove modules
stand alone. In both cases resolution fails with nothing to fall back on.

**4. Both.** `go.work` for development, plus a `replace` in each module as the
fallback.

## Decision

Option 4. Three modules — `shared`, `backend`, `agent` — with:

```
go.work                                          # use ./shared ./backend ./agent
backend/go.mod  replace …/shared => ../shared
agent/go.mod    replace …/shared => ../shared
```

The two mechanisms cover different situations and are not redundant:

| Context | Resolves via |
| --- | --- |
| Local development, IDE | `go.work` |
| `make` targets and CI (`GOWORK=off`) | `replace` |
| Docker build (no `go.work` copied) | `replace` |
| A clone of one module's directory | `replace` |

CI runs **every** Go command with `GOWORK=off`, per module, in its own job. That is
what mechanically enforces "every module must compile independently" — a module that
only builds because a sibling happens to be on disk fails there rather than in a
Docker build or on a contributor's machine.

A separate CI job then verifies the workspace itself resolves, since that is what
developers actually use. Both properties are checked because either alone can break.

`dashboard/` is npm, not Go, so it is outside this arrangement entirely.

## Consequences

**Gained**

- Genuinely independent dependency graphs. The agent's `go.mod` does not mention
  Fiber, and cannot come to mention it by accident.
- Independent compilation is enforced by CI, not asserted in a document.
- The Docker build needs no workspace file, which keeps `.dockerignore` able to
  exclude `go.work` and keeps the build context minimal.
- Developers get working IDE navigation across modules with no per-module setup.

**Accepted costs**

- Three `go.mod` files and three `go.sum` files to keep tidy. `make tidy` handles it,
  and CI fails on an untidy one.
- Adding a module means editing `go.work` **and** adding a `replace` — two steps that
  are easy to half-complete. `make verify-modules-standalone` catches the omission.
- The `replace` directives mean these modules cannot be `go install`ed by version from
  a proxy. That is fine: `shared` is not intended for external consumption, and the
  binaries are distributed as release artifacts rather than via `go install`.
- Local development and CI resolve dependencies by different mechanisms. That is a
  divergence worth being uneasy about, which is exactly why `make` also defaults to
  `GOWORK=off` — so the CI path is the one exercised most often, and the workspace is
  the special case rather than the default.

## Revisit if

- `shared` becomes something external consumers should import by version, at which
  point it needs publishing and the `replace` directives become a problem.
- Go gains first-class monorepo support that makes one mechanism sufficient.
