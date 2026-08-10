# ADR-0005: ULID identifiers rather than UUIDv4 or serial integers

**Status:** Accepted · Phase 1

## Context

Beuvian needs primary keys for `users`, `devices`, `repositories`, `sessions`,
`prompt_queue`, `messages`, `notifications`, and more. The identifiers appear in three
places with different demands:

1. **PostgreSQL primary keys and indexes**, where insert locality affects write
   performance.
2. **The wire protocol and REST API**, where they are visible to clients and appear in
   support conversations.
3. **Logs**, where a human reads them out of a terminal and types them into a query.

The agent must also be able to generate a device ID **before** it has ever contacted
the backend, since registration is what it uses the ID for.

[ADR-0003](0003-shared-module-is-protocol-only.md) forbids a dependency in `shared`,
which rules out the obvious ULID libraries.

## Options considered

**1. `BIGSERIAL`.** Compact (8 bytes), perfectly ordered, ideal index locality. Two
disqualifying problems: the database must assign it, so the agent cannot mint a device
ID offline; and sequential public IDs leak business information and invite enumeration
— `/v1/devices/5` tells an attacker there are at least five devices and that four more
exist to try.

**2. UUIDv4.** Client-generatable, no coordination, universally understood. But it is
random across its whole range, so inserts scatter across the B-tree: each one touches a
different page, the cache hit rate collapses, and the index fragments. It is also
unordered, so "most recent sessions" needs a separate timestamp column and index, and
it is unreadable aloud — 36 characters with ambiguous hex.

**3. UUIDv7.** Time-ordered UUID, which fixes v4's locality problem and is the modern
answer. Genuinely strong. Its drawbacks here are presentational: 36 characters with
hyphens, hex alphabet, and no natural place for a type prefix.

**4. ULID.** 48-bit millisecond timestamp plus 80 bits of randomness, rendered as 26
characters of Crockford base32. Time-ordered like v7, more compact textually, and the
alphabet excludes ambiguous characters.

## Decision

Option 4, implemented in `shared/id` against `crypto/rand` and the standard library
only, with entity prefixes:

```
usr_01J9Z3K7QF8XKM2N4P6R8T0VWY
dev_01J9Z3K7QF8XKM2N4P6R8T0VWY
ses_01J9Z3K7QF8XKM2N4P6R8T0VWY
prm_01J9Z3K7QF8XKM2N4P6R8T0VWY
```

Three properties are doing the work:

**Time ordering gives index locality.** New rows sort to the right edge of the B-tree,
so inserts touch the same few pages instead of scattering. It also means "the most
recent sessions" is `ORDER BY id DESC` with no extra column or index.

**Crockford base32 excludes I, L, O, and U.** Users read device IDs out of logs and
into support tickets. Excluding the characters that get confused with 1 and 0 removes an
entire category of transcription error, and it is free.

**Prefixes make values self-describing.** An ID that leaks into a log line or error
message announces what it is, and passing a session ID where a device ID belongs is
visible on inspection rather than silent. `Validate()` checks the shape at trust
boundaries, so a malformed value fails with a clear error rather than as an opaque
driver error.

### One deliberate exception

`session_logs` uses `BIGSERIAL`, not a ULID. It takes by far the highest insert rate,
and 8 bytes versus 26 is material across millions of rows in both table and index size.
Log rows are never referenced by external clients, so the self-describing-ID argument
does not apply. The general rule is not worth a measurable cost where its benefits are
absent.

## Consequences

**Gained**

- Index locality without the database assigning keys.
- The agent generates its device ID offline, before registration.
- Sortable by creation time with no extra column.
- No enumerable public IDs.
- Readable and dictatable over a phone call.
- The creation timestamp is recoverable from the ID alone, which is useful for expiry
  checks and debugging with no database round trip.

**Accepted costs**

- **A hand-written encoder.** About 40 lines that `oklog/ulid` provides for free,
  required by ADR-0003. Mitigated by tests covering shape, alphabet, uniqueness across
  20,000 IDs, sort order, timestamp recovery, and rejection of malformed input.
- **Larger keys than integers.** 26 characters (30 with a prefix) stored as `TEXT`
  versus 8 bytes. A real cost in table and index size, accepted for every table except
  the one where it would hurt most.
- **The timestamp is public.** A ULID reveals when the entity was created, to the
  millisecond. For Beuvian's entities that is not sensitive, but it is genuinely
  disclosed and would matter for something like an invite token — which is why
  `id.Nonce()` is a separate function with no embedded timestamp.
- **Millisecond collision risk in theory.** Two IDs in the same millisecond rely on 80
  random bits to differ. The collision probability is negligible, and unlike a
  monotonic-counter ULID implementation this one makes no ordering guarantee *within* a
  millisecond.
- **Not a UUID.** `id` is `TEXT`, not `UUID`, so PostgreSQL's UUID functions and
  operators do not apply, and a tool expecting UUID-shaped keys will not recognise these.

## Revisit if

- PostgreSQL's `UUID` type becomes worth the loss of prefixes and the readable
  alphabet — for instance if an external system requires UUID-shaped keys. UUIDv7 would
  then be the replacement, not v4.
- A table other than `session_logs` reaches a volume where 26-byte keys measurably
  matter, in which case the same exception applies there.
