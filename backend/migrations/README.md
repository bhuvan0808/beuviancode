# Database migrations

SQL migrations for Beuvian's Supabase PostgreSQL database. **Populated in Phase 2**
alongside the schema.

## Naming

```
NNNN_short_description.up.sql
NNNN_short_description.down.sql
```

Sequential, zero-padded, paired up/down. Example:

```
0001_create_users.up.sql
0001_create_users.down.sql
```

## Why plain SQL rather than Supabase's migration tooling

PROJECT.md names Supabase as the database, but plain versioned SQL keeps the schema
portable. Nothing about Beuvian's schema is Supabase-specific — it is ordinary
PostgreSQL — and using vendor tooling for it would create a lock-in that the Clean
Architecture boundaries otherwise avoid. Plain SQL also runs identically against
the local Postgres container, so a developer's database and production are migrated
by the same files.

## Why migrations do not run at application startup

`database.auto_migrate` defaults to `false`, and production leaves it off.

During a rolling deploy several backend instances start at once. If each migrated
on boot they would race: at best one wins and the others error, at worst two
concurrent DDL statements deadlock and the deploy wedges with the database
half-migrated. Migrations therefore run as an explicit, single-instance CI step
before new application containers are promoted.

The flag exists because it is genuinely convenient for local development, where
there is exactly one instance and no such race.

## Rules

- **Forward-compatible first.** A migration must leave the *previous* version of
  the application working, because during a rolling deploy both versions run
  simultaneously against the same schema. Adding a `NOT NULL` column with no
  default breaks the old code instantly.
- **Expand then contract.** To rename or drop a column: add the new one and
  dual-write, deploy, backfill, switch reads, deploy, and only then drop the old
  one in a later migration.
- **Never edit a migration that has been applied anywhere.** Write a new one. An
  edited migration means two databases silently disagree about their schema.
- **Every `up` needs a working `down`.** An untested rollback is not a rollback.
- **Index concurrently in production.** `CREATE INDEX` takes a write lock on the
  table; `CREATE INDEX CONCURRENTLY` does not. On a table with real traffic the
  difference is an outage.

## Required tables

Per PROJECT.md, with indexes, foreign keys, constraints, `created_at`,
`updated_at`, and soft deletes where appropriate:

`users` · `devices` · `repositories` · `sessions` · `session_logs` · `messages` ·
`notifications` · `prompt_queue` · `agent_status` · `user_settings` ·
`oauth_accounts` · `refresh_tokens`

See [`docs/DATABASE.md`](../../docs/DATABASE.md) for the schema design and the
reasoning behind it.
