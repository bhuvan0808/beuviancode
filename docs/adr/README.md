# Architecture Decision Records

PROJECT.md requires that every architectural decision be explained. These are the
decisions that had genuine alternatives, recorded with their reasoning and — most
importantly — the downsides accepted.

An ADR that lists no drawbacks is not describing a real decision. If a future reader
concludes the reasoning no longer holds, that is grounds to supersede the record
rather than to quietly diverge from it.

| # | Decision | Status |
| --- | --- | --- |
| [0001](0001-clean-architecture-layering.md) | Clean Architecture layering in the backend | Accepted |
| [0002](0002-go-workspace-multi-module.md) | Three Go modules with a workspace and replace directives | Accepted |
| [0003](0003-shared-module-is-protocol-only.md) | The shared module has zero third-party dependencies | Accepted |
| [0004](0004-configuration-precedence-engine.md) | One reflection-based configuration precedence engine | Accepted |
| [0005](0005-ulid-identifiers.md) | ULID identifiers rather than UUIDv4 or serial integers | Accepted |
| [0006](0006-prompt-queue-postgres-authoritative.md) | PostgreSQL is authoritative for the prompt queue; Redis accelerates | Accepted |
| [0007](0007-pluggable-coding-adapter.md) | A registry-based pluggable coding-agent adapter | Accepted |

## Format

Each record states:

- **Context** — the forces at play, including the constraints from PROJECT.md
- **Options considered** — with the case for each, not strawmen
- **Decision**
- **Consequences** — the benefits *and* the costs accepted
- **Revisit if** — the observable condition that would justify reopening it

## Writing a new one

Copy the shape of an existing record. Number sequentially. Add it to the table above.

Write one when a change involves a choice a reasonable engineer might have made
differently. Do not write one for choices with a conventional default and no real
alternative — that is noise that makes the genuine decisions harder to find.
