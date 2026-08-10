# ADR-0006: PostgreSQL is authoritative for the prompt queue; Redis accelerates

**Status:** Accepted · Phase 1

## Context

PROJECT.md contains an apparent contradiction that has to be resolved before any code
is written:

- The database section lists a **`prompt_queue` table**.
- The Redis section lists **"Prompt Queue"** among Redis's uses.
- The Redis section also states: **"Never store permanent business data."**

So a prompt queue is specified in both stores, while Redis is forbidden from holding
permanent business data. Either the two lines are redundant, or they describe different
roles for the same data.

What is at stake makes this worth getting right. A prompt is a user's instruction,
often typed on a phone away from the machine, and often while the device is offline
and cannot receive it yet. Losing one is not a cache miss — it is losing the thing the
user asked for, silently, with no way for them to know it happened.

## Options considered

**1. Redis only.** Fast, purpose-built, with native list and stream primitives. But
Upstash is a managed cache: a restart, an eviction, or a plan limit loses the queue. It
also directly violates PROJECT.md's own prohibition, since a pending prompt is exactly
permanent business data until delivered.

**2. PostgreSQL only.** Durable, transactional, and simple — one store, one truth. The
cost is delivery latency: with no push channel, dispatch relies on polling. A 1-second
poll across every connected device is constant wasted load; a 5-second poll makes the
product feel sluggish precisely at its most important moment. `LISTEN`/`NOTIFY` could
replace polling but does not survive a connection drop, so a missed notification still
needs a polling backstop.

**3. Redis as the queue, PostgreSQL as an audit log.** Redis is authoritative for
delivery; PostgreSQL records history. Fast, but the failure mode is that a Redis
restart loses pending prompts while PostgreSQL shows them as having existed. Data that
is recorded but undeliverable is the worst of both.

**4. PostgreSQL authoritative, Redis as a dispatch accelerator.** The prompt is
committed to PostgreSQL before the user is acknowledged, then published to Redis for
immediate delivery. Redis loss costs latency; it cannot cost data.

## Decision

Option 4. Both lines in PROJECT.md are honoured, with distinct roles:

| | PostgreSQL `prompt_queue` | Redis |
| --- | --- | --- |
| Role | System of record | Dispatch accelerator |
| Durability | Required | Disposable |
| Loss impact | Data loss | Added latency only |

### The write path, and why the ordering matters

```
POST /v1/prompts
   1. INSERT INTO prompt_queue (status='pending')   ← committed
   2. PUBLISH to Redis for immediate dispatch       ← best effort
   3. 202 Accepted
```

Step 1 completes before step 3. That ordering is the entire guarantee: once the user
sees `202`, the prompt exists durably. If step 2 fails, the response is unchanged and
the prompt is delivered from PostgreSQL on the next agent reconnect or reconciliation
sweep.

Reversing steps 1 and 2 — publishing first for speed — would create a window where the
user is told their prompt was accepted and the only copy is in a cache. That window is
small and would be hit rarely, which is what makes it the kind of bug that survives to
production.

The status column carries the lifecycle: `pending → dispatched → delivered`, with
`failed` and `cancelled` as terminals. The agent echoes `prompt_id` in its `ACK`, which
is what allows `delivered` to be recorded exactly once.

### Consequences elsewhere in the design

Three configuration and infrastructure choices follow directly from this decision, and
they are not independent preferences:

- **`redis.required` defaults to `false`.** If Redis is a hard dependency the backend
  dies when the cache is unavailable, which converts a latency problem into an outage.
  With PostgreSQL authoritative, degrading is strictly better.
- **`maxmemory-policy noeviction`** in the compose file. `allkeys-lru` would let Redis
  silently evict a queued prompt under memory pressure. For a queue, a hard failure is
  correct and eviction is not — even though the prompt would still be recoverable from
  PostgreSQL, an eviction hides the problem.
- **An offline device is not an error** on `POST /v1/prompts`. The prompt is queued
  durably and delivered on reconnect, so the user gets `202` regardless. This is the
  behaviour the product depends on: a developer sends a prompt from a train, and the
  laptop picks it up when it wakes.

## Consequences

**Gained**

- A user's instruction cannot be lost by a Redis restart, eviction, or plan limit.
- Push-speed delivery when Redis is healthy, with no polling in the common path.
- The backend survives Redis being unavailable.
- The queue is queryable with SQL, so "what is pending for this device" needs no
  special tooling.
- PROJECT.md's prohibition holds literally: Redis carries a transient dispatch signal,
  never the only copy.

**Accepted costs**

- **Two writes per prompt.** More work than either option alone, on the product's
  hottest path. Acceptable: prompt volume is human-paced, a handful per session, not a
  firehose.
- **They can disagree.** Redis may hold a dispatch message for a row PostgreSQL has
  marked delivered. Handled by making the agent deduplicate on envelope `id` and by
  treating PostgreSQL as the tiebreak in every case.
- **A reconciliation sweep is required.** Something must find `pending` rows whose
  Redis message was lost. That is a periodic job that would not exist under option 1 or
  2, and it is genuinely additional machinery.
- **More moving parts.** Two stores in the path of the core operation means two things
  that can be misconfigured.

## Revisit if

- Prompt volume grows enough that two writes per prompt is a measurable bottleneck —
  unlikely while prompts are typed by humans.
- Redis Streams with persistence is adopted, which would make option 3 durable enough
  to reconsider. Even then, the reconciliation-free simplicity would have to outweigh
  losing SQL queryability.
