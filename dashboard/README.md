# Beuvian Dashboard

Next.js · React · TypeScript · TailwindCSS

**Status: Phase 4.** This directory is intentionally a placeholder. Scaffolding a
half-built Next.js app in Phase 1 would add churn and a `node_modules` tree to a
repository whose Phase 1 deliverable is architecture, and it would have to be
substantially rewritten once the real API surface exists.

## What it will be

The dashboard is how a developer controls a coding session from a phone. That framing
drives the design: **mobile is the primary target**, not a responsive afterthought.
The desktop layout is the adaptation, because the whole point is that the user is *not*
at their desk.

### Pages

| Page | Purpose |
| --- | --- |
| Login | GitHub OAuth |
| Dashboard | Devices, current session, current task, connection status at a glance |
| Devices | List, online/offline, register, revoke |
| Device Details | Platform, agent version, capabilities, CPU, memory, history |
| Repositories | Manage repositories per device |
| Live Session | Realtime terminal, prompt box, stop session |
| Session History | Past sessions with their transcripts |
| Notifications | Task complete, waiting for input, device offline |
| Settings | Notification preferences, theme, log retention |

### Live Session

The screen the product lives or dies on:

- Realtime terminal with auto-scroll, ordered by `seq` from the protocol
- Prompt textbox → `POST /v1/prompts`
- Live status: state, elapsed runtime, CPU, memory, queued prompt count
- Reconnect and stop controls
- Placeholders for git diff, screenshot preview, and deployment status

## Contracts it must honour

The backend contracts are already fixed and documented, so Phase 4 implements against
a defined surface rather than negotiating one:

- [WEBSOCKET_PROTOCOL.md](../docs/WEBSOCKET_PROTOCOL.md) — envelope, 13 message types,
  `AgentState`, reconnection with backoff and jitter
- [API.md](../docs/API.md) — REST surface, error shape, cursor pagination

Points the dashboard client must get right, all of which are consequences of decisions
already made:

- **Order log lines by `seq`**, not by timestamp. Timestamps collide under load and are
  not monotonic across a clock adjustment.
- **Honour `truncated`.** A truncated transcript must be visibly marked, or it reads as
  complete.
- **Expect to be disconnected.** A slow consumer is dropped deliberately so one stalled
  phone cannot stall the broadcaster. Reconnect with backoff and jitter, and request
  history from `after_seq` on reconnect.
- **Render from `state` alone.** It is a single enum for exactly this reason; do not
  derive display state from a combination of fields.
- **Distinguish `graceful` on `DEVICE_OFFLINE`** — "went offline" and "lost connection"
  mean different things to a user trying to work out whether something is broken.

## Environment

```bash
NEXT_PUBLIC_BEUVIAN_API_URL=http://localhost:8080
NEXT_PUBLIC_BEUVIAN_WS_URL=ws://localhost:8080/v1/ws
```

`NEXT_PUBLIC_` variables are inlined into the browser bundle at build time and are
therefore **public**. Never put a secret behind that prefix.

The dashboard's origin must be listed in the backend's `BEUVIAN_CORS_ALLOWED_ORIGINS`.
A wildcard will not work: the refresh cookie is credentialed, and browsers reject a
wildcard origin with credentials.
