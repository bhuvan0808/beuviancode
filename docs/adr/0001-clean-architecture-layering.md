# ADR-0001: Clean Architecture layering in the backend

**Status:** Accepted · Phase 1

## Context

PROJECT.md requires Clean Architecture, SOLID, modularity, extensibility, and easy
testability, and it names specific vendors: Supabase PostgreSQL, Upstash Redis,
Fiber, GitHub OAuth.

Those two requirements pull in the same direction, which is worth noticing. Every
named vendor is one a team might plausibly replace — a company outgrowing Supabase's
connection limits, a self-hoster who wants plain PostgreSQL, a decision to move off
Fiber. The question is whether that replacement is a new file or a rewrite.

There is also a test-cost question. The backend's interesting logic is authorisation,
prompt queueing, session state, and token rotation. If exercising any of it requires
a running PostgreSQL and Redis, the tests are slow and environment-dependent, and
tests that are expensive to run stop being run.

## Options considered

**1. Layered package structure, no interface indirection.** `handlers` → `services`
→ `repositories`, each importing the next concretely. Simple, familiar, and the
common Go idiom. But a service importing the repository package imports pgx
transitively, so a service test needs a database, and swapping the database means
editing every service.

**2. Clean Architecture with ports and adapters.** Four layers, dependencies pointing
inward, interfaces declared beside their consumer. More indirection, and the cost is
real: a reader tracing a request passes through an interface boundary where option 1
would have a direct call.

**3. Hexagonal architecture with an explicit application core.** Effectively the same
dependency discipline as option 2 with different vocabulary and, usually, more
ceremony around command and query objects. The extra structure buys little at this
size.

## Decision

Option 2, with four layers:

```
adapter  ───▶  port  ◀───  app  ───▶  domain
```

| Layer | May import | Contains |
| --- | --- | --- |
| `domain` | stdlib, `shared/protocol` | Entities, invariants, domain errors |
| `port` | `domain` | Interfaces the use cases depend on |
| `app` | `domain`, `port` | Use cases, orchestration, authorisation |
| `adapter` | `domain`, `port`, `app` | Fiber, pgx, Redis, OAuth, WebSocket |

Interfaces live in `port`, next to their **consumer**. Go's implicit interface
satisfaction means an adapter never imports `port` — it simply has the right method
set — so the dependency arrow points inward with no ceremony.

Everything sits under `backend/internal/`, which is a language feature rather than a
convention: nothing outside the module can import these packages, so the layering
cannot be bypassed by an external consumer.

## Consequences

**Gained**

- Replacing Supabase or Upstash touches only `internal/adapter`. That is the concrete
  payoff, against a likely event.
- Use cases are testable with in-memory fakes — no database, no container, no
  network, no mocking framework.
- One use case serves several entry points. The same `ForwardPrompt` runs from the
  dashboard's REST call today and from the planned public API and WhatsApp
  integration later, which is what PROJECT.md means by "no duplicated business logic".
- `Clock` and `IDGenerator` as ports make time-dependent rules (token expiry, stale
  sessions) testable without sleeping.

**Accepted costs**

- More indirection. Tracing a request means passing through an interface, and for
  simple CRUD that genuinely is more files for the same behaviour.
- Adapters define their own DTOs rather than serialising domain entities. This looks
  like duplication. It is not — the wire format is a public contract, and marshalling
  entities directly makes every internal rename a breaking API change and publishes
  every new field by accident — but it is real typing.
- The discipline is not compiler-enforced. Nothing stops someone importing `adapter`
  from `app`; only review and the `doc.go` contracts do.

**Mitigations**

- Each layer's `doc.go` states its contract explicitly and says what must not appear.
- Interfaces are kept narrow, so a fake costs little to write.

## Revisit if

- Layer-crossing violations appear repeatedly in review, suggesting the boundaries do
  not match how the code actually wants to be organised.
- An import-boundary linter becomes available that could enforce this mechanically,
  in which case add it rather than relying on review.
