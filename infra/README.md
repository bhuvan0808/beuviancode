# Infrastructure

Deployment configuration. Full deployment is **Phase 6**; see
[docs/DEPLOYMENT.md](../docs/DEPLOYMENT.md) for the plan.

```
infra/
├── railway/      Backend service configuration
└── supabase/     Project notes and SQL helpers
```

## What is and is not version-controlled here

Most of Beuvian's deployment configuration is environment variables in a platform
dashboard, and that is the right place for it — secrets must not be in git, and the
platforms are the source of truth for their own settings.

This directory holds what can meaningfully be versioned: service definitions,
non-secret platform settings, and SQL helpers. Anything holding a credential belongs in
the platform's environment, never here.

## Target topology

| Component | Platform | Notes |
| --- | --- | --- |
| Backend | Railway | Container from `docker/backend.Dockerfile`, built from the repo root |
| Database | Supabase PostgreSQL | Pooler endpoint (6543), `sslmode=require` |
| Cache / queue | Upstash Redis | TCP endpoint — pub/sub is needed for cross-instance fan-out |
| Dashboard | Vercel | Root directory `dashboard/` |
| Agent | User's machine | Release binaries from GitHub Releases |

## Region placement matters more than it looks

Put Supabase, Upstash, and Railway in the **same region**. Every query and every
dispatch pays the round trip, and a cross-continent pairing is the easiest way to make
the entire product feel slow while every individual component looks healthy.

## Connection budget

`instances × BEUVIAN_DB_MAX_OPEN_CONNS` must stay under Supabase's connection cap.
Exceeding it does not degrade gradually — every instance begins failing at once, which
presents as a total outage rather than as a capacity problem. Use the pooler endpoint
and size the pool deliberately.
