# Seed data

Seed scripts for local development and integration tests. **Populated in Phase 2**,
after the schema exists.

## Purpose

A developer should be able to reach a useful state without clicking through GitHub
OAuth and registering a device by hand. Seeds create a known user, a device, a
repository, and a finished session with a transcript, so the dashboard has something to
render on first load.

## Rules

- **Development and test only.** Seeds must be impossible to run against production —
  guarded by refusing to execute when `BEUVIAN_ENV=production`, not merely by
  convention.
- **Idempotent.** Re-running must not duplicate rows. Use deterministic IDs so a second
  run is a no-op rather than a second copy of everything.
- **No real credentials.** Device tokens are stored hashed, exactly as in production;
  seeded tokens are obviously fake values with no privileges elsewhere.
- **Separate from migrations.** A migration changes the schema for everyone; a seed
  inserts data for one developer. Mixing them means production runs the seed.

## Planned

| Script | Contents |
| --- | --- |
| `dev_user.sql` | One user with settings and an OAuth account |
| `dev_device.sql` | One registered device, online, with `claude` in its capabilities |
| `dev_repository.sql` | One repository linked to that device |
| `dev_session.sql` | One completed session with log lines and messages |

Integration tests will use a separate minimal fixture rather than these, since a test
that depends on developer-convenience data becomes fragile the moment that data changes
for a UI reason.
