# Deployment Guide

Backend on **Railway**, dashboard on **Vercel**, database on **Supabase**, cache on
**Upstash**, agent as a signed binary on the user's own machine.

Full end-to-end deployment is **Phase 6**. This document is the plan, written now so
Phase 1's Docker and CI choices can be checked against it rather than rediscovered.

---

## Overview

```
 GitHub tag v*.*.*
        │
        ├──▶ CI: test all modules ──▶ build agent ×6 ──▶ GitHub Release
        │                          └─▶ backend image ──▶ ghcr.io
        │
        ├──▶ Railway  ← backend container
        │       ├── Supabase PostgreSQL
        │       └── Upstash Redis
        │
        └──▶ Vercel   ← dashboard (Phase 4)
```

---

## 1. Supabase (PostgreSQL)

1. Create a project. Choose the region closest to your Railway region — every query
   pays this round trip, and a cross-continent pairing is the single easiest way to
   make the whole product feel slow.
2. Copy the connection string from **Project Settings → Database**.
3. **Use the pooler endpoint (port 6543), not the direct one (5432).**

```
BEUVIAN_DB_URL=postgres://postgres.PROJECT:PASSWORD@aws-0-REGION.pooler.supabase.com:6543/postgres?sslmode=require
```

The pooler is not optional at more than one instance. Supabase caps direct
connections; with N backend instances each opening `max_open_conns`, that cap is
exhausted and **every** instance begins failing at once — which presents as a total
outage rather than as a configuration mistake.

Size the pool so `instances × max_open_conns < Supabase's limit`. With the default 25
and a limit of 60, three instances already overcommit.

`sslmode=require` is mandatory: the connection crosses the public internet.

### Migrations

Run as an explicit step **before** promoting new containers, never from application
startup. During a rolling deploy multiple instances would race, and two concurrent
DDL statements can deadlock with the schema half-applied.

```bash
migrate -path backend/migrations -database "$BEUVIAN_DB_URL" up
```

---

## 2. Upstash (Redis)

1. Create a database in the same region as Railway.
2. Copy the Redis (TCP) connect URL — `rediss://`, TLS.

```
BEUVIAN_REDIS_URL=rediss://default:PASSWORD@REGION.upstash.io:6379
BEUVIAN_REDIS_KEY_PREFIX=beuvian:prod:
```

Set a distinct `key_prefix` per environment. Staging and production can then share an
instance without either flushing the other's keys.

Use the **TCP** endpoint rather than the REST API: the gateway needs pub/sub for
cross-instance fan-out, and pub/sub is a connection-oriented feature.

### Sizing note

Redis holds only ephemeral data — presence, heartbeat, the hot dispatch queue,
rate-limit counters, locks, short cache. It is never the system of record. If Upstash
is unavailable, prompts still persist to PostgreSQL and deliver on reconnect, which
is why `BEUVIAN_REDIS_REQUIRED` should stay `false`: the backend degrades instead of
dying.

---

## 3. GitHub OAuth

**Settings → Developer settings → OAuth Apps → New OAuth App**

| Field | Value |
| --- | --- |
| Homepage URL | `https://app.beuvian.example` |
| Authorization callback URL | `https://api.beuvian.example/v1/auth/github/callback` |

The callback must match **exactly** — scheme, host, and path. A trailing-slash
mismatch is rejected by GitHub with an error that does not explain itself, and it is
the most common setup failure.

Use **separate OAuth apps** for staging and production. Sharing one means a staging
misconfiguration can redirect production users.

---

## 4. Railway (backend)

Deploy the image built by CI, or point Railway at the repository with:

- Dockerfile path `docker/backend.Dockerfile`
- **Build context: the repository root** — the image needs `backend/` and `shared/`

### Environment variables

```bash
BEUVIAN_ENV=production
# PORT is injected by Railway; config adopts it into the normal precedence chain,
# so an explicit --port flag would still win.

BEUVIAN_DB_URL=postgres://…pooler.supabase.com:6543/postgres?sslmode=require
BEUVIAN_REDIS_URL=rediss://default:…@….upstash.io:6379
BEUVIAN_REDIS_KEY_PREFIX=beuvian:prod:

BEUVIAN_AUTH_JWT_SECRET=<openssl rand -base64 48>
BEUVIAN_AUTH_GITHUB_CLIENT_ID=…
BEUVIAN_AUTH_GITHUB_CLIENT_SECRET=…
BEUVIAN_AUTH_GITHUB_CALLBACK_URL=https://api.beuvian.example/v1/auth/github/callback
BEUVIAN_AUTH_DASHBOARD_URL=https://app.beuvian.example

BEUVIAN_CORS_ALLOWED_ORIGINS=https://app.beuvian.example
BEUVIAN_LOG_LEVEL=info
BEUVIAN_LOG_FORMAT=json
```

The backend **refuses to start** in production if any of these is wrong. Validation
rejects plaintext `http://` origins, `cookie_secure: false`, a signing secret under
32 bytes, and `log.level: debug`. That is intentional: each of those is a security
defect that would otherwise run silently for months.

Verify an environment before it takes traffic:

```bash
docker run --rm --env-file prod.env ghcr.io/OWNER/beuvian/backend:v0.1.0 -check
```

### Health checks

| Path | Purpose |
| --- | --- |
| `/health` | Liveness. No dependency checks |
| `/health/ready` | Readiness. Checks database and Redis |

Point Railway's healthcheck at **`/health/ready`**, and be sure liveness stays
dependency-free: if liveness checked the database, a brief Supabase blip would make
Railway kill healthy instances and escalate a partial degradation into an outage.

### Graceful shutdown

`BEUVIAN_SERVER_SHUTDOWN_GRACE` defaults to 15s and validation caps it at 30s,
because Railway sends `SIGKILL` 30s after `SIGTERM`. A longer grace is not more
graceful — it is truncated mid-drain, which is worse than a shorter one that
completes. The lifecycle supervisor stops the HTTP server before the pools it depends
on, so in-flight requests finish rather than failing on every deploy.

### Scaling and WebSockets

Scaling past one instance requires cross-instance fan-out: an agent connected to
instance A must reach a dashboard connected to instance B. Redis pub/sub carries
those events, which is why the TCP endpoint is required.

Sticky sessions are **not** needed — the agent reconnects with exponential backoff
and jitter, so landing on a different instance is the normal case rather than an
error. Jitter matters here specifically: without it, a deploy makes every agent
reconnect simultaneously and the herd keeps the new instance down.

---

## 5. Vercel (dashboard) — Phase 4

```bash
NEXT_PUBLIC_BEUVIAN_API_URL=https://api.beuvian.example
NEXT_PUBLIC_BEUVIAN_WS_URL=wss://api.beuvian.example/v1/ws
```

`NEXT_PUBLIC_` variables are inlined into the browser bundle at build time and are
therefore **public**. Never put a secret behind that prefix.

Set the root directory to `dashboard/`. Add the Vercel domain to
`BEUVIAN_CORS_ALLOWED_ORIGINS`, including preview domains if previews should reach
the real API — and prefer pointing previews at staging instead.

---

## 6. Agent distribution

CI builds six binaries on every `v*.*.*` tag with SHA256 checksums:

```
beuvian-agent-windows-amd64.exe    beuvian-agent-darwin-arm64
beuvian-agent-windows-arm64.exe    beuvian-agent-linux-amd64
beuvian-agent-darwin-amd64         beuvian-agent-linux-arm64
```

Builds are reproducible: version metadata comes from the commit date rather than the
wall clock, so re-running the workflow produces identical binaries.

### User setup

```bash
beuvian-agent -version    # confirm the download
beuvian-agent -detect     # confirm Claude Code is on PATH
beuvian-agent -check      # confirm configuration
beuvian-agent
```

Configuration goes in the OS-appropriate location:

| | |
| --- | --- |
| Windows | `%AppData%\Beuvian\config.yaml` |
| macOS | `~/Library/Application Support/Beuvian/config.yaml` |
| Linux | `~/.config/beuvian/config.yaml` |

```yaml
backend:
  url: wss://api.beuvian.example/v1/ws
  api_url: https://api.beuvian.example
coding:
  adapter: claude
  working_directory: /home/you/src/your-project
log:
  file_path: /home/you/.local/state/beuvian/agent.log
```

Set `log.file_path`. When a user reports a problem, whatever was on stdout is long
gone.

### Not yet addressed

**Code signing.** Unsigned binaries trigger SmartScreen on Windows and Gatekeeper on
macOS, and telling users to click through a security warning is a poor first
impression for software that then asks for machine access. This needs an
Authenticode certificate and an Apple Developer ID plus notarisation. Deferred to
Phase 6, and it is a launch blocker rather than a nice-to-have.

**Auto-update.** The agent is designed to support it (`shared/version` reports the
running build) but it is not implemented.

---

## 7. Secrets

| Secret | Lives in | Rotation |
| --- | --- | --- |
| `BEUVIAN_AUTH_JWT_SECRET` | Railway env | Rotating invalidates all access tokens; refresh tokens survive, so users are not logged out |
| GitHub OAuth client secret | Railway env | Rotate in the OAuth app, then Railway |
| Supabase password | Railway env | Rotate in Supabase, then Railway |
| Upstash password | Railway env | |
| Device tokens | Agent encrypted state | Per device; revoke via the dashboard |

Secrets live in the platform's environment, never in a committed file and never in a
Docker build context — anything copied into a build context is recoverable from the
image layers even if a later stage deletes it, which is why `.dockerignore` excludes
them rather than relying on cleanup.

---

## 8. Deployment checklist

**Before the first deploy**

- [ ] Supabase and Upstash in the same region as Railway
- [ ] Pooler endpoint (6543), `sslmode=require`
- [ ] `instances × max_open_conns` under Supabase's connection cap
- [ ] Separate GitHub OAuth apps for staging and production
- [ ] `openssl rand -base64 48` for the JWT secret
- [ ] `-check` passes against the production environment
- [ ] Migrations applied
- [ ] Railway healthcheck on `/health/ready`
- [ ] CORS lists only the real dashboard origin

**Every deploy**

- [ ] CI green
- [ ] Migrations applied before containers are promoted
- [ ] Migration is backward-compatible with the previous app version (both run during
      a rolling deploy)
- [ ] `/health/ready` returns 200 after rollout
- [ ] Agents reconnect — check for a `DEVICE_ONLINE` burst

**Rollback**

- [ ] Redeploy the previous image tag
- [ ] Only roll the schema back if the new migration is genuinely incompatible;
      prefer rolling forward, since a `down` migration on live data is riskier than
      the bug it undoes
