<div align="center">

# Beuvian

**The open-source AI coding agent operating system.**

Control any coding agent from anywhere — from your phone, your browser, or another machine.

[![CI](https://github.com/bhuvan0808/beuviancode/actions/workflows/ci.yml/badge.svg)](https://github.com/bhuvan0808/beuviancode/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)
[![Built with 💙 on Go](https://img.shields.io/badge/built_with-%F0%9F%92%99%20on%20Go-00ADD8.svg)](https://go.dev)

*You keep using — and paying for — your own coding agent.
Beuvian is the operating system that runs around it.*

</div>

---

## What is Beuvian?

Your AI coding agent works for 45 minutes and finishes. You are not at your desk.

Beuvian tells your phone the moment it stops, shows you what it did, and lets you
type the next instruction. The Desktop Agent injects that prompt locally and your
coding agent carries on. You never walk back to the laptop.

```
You start Claude Code  →  it works  →  it finishes
                                          ↓
                              Beuvian updates your dashboard
                                          ↓
          You open Beuvian on your phone: "Now implement authentication."
                                          ↓
              Desktop Agent injects the prompt  →  coding agent continues
```

Beuvian is the **control plane** for your coding agents: remote control, session
management, live monitoring, notifications, and prompt forwarding — the parts that
are missing when you are away from the machine.

### Why "an operating system"?

A real OS manages *processes* — their lifecycles, scheduling, communication, and
resources. Beuvian does the same for **coding agents**:

| OS concept | Beuvian |
| ---------- | ------- |
| Process | A coding-agent session (Claude Code, Codex, Gemini, …) |
| Scheduling | The prompt queue — Postgres-authoritative, Redis-accelerated |
| Inter-process communication | Versioned WebSocket protocol + REST |
| Process table | Live session state machine, monitoring from anywhere |
| Kernel | The backend gateway, in Clean Architecture layers |
| Device drivers | Pluggable adapter registry — one file per coding agent |

### What Beuvian is not

This matters enough to state plainly:

- Beuvian **never** calls Anthropic, OpenAI, or any other model API.
- Beuvian **never** replaces your coding agent — and never sees your provider API keys.
- Beuvian **never** ships an IDE. It runs *beside* the agent, not instead of it.

Your agent writes the code. Beuvian gives you the cockpit.

---

## Status

**Phase 1 of 7 complete.** The foundation is built, verified, and green on CI; the
product does not function end to end yet.

| Phase | Scope | Status |
| ----- | ----- | ------ |
| 1 | Architecture, monorepo, configuration, Docker, CI/CD, documentation | ✅ Complete |
| 2 | Backend: Fiber, auth, database, Redis, WebSocket gateway | ⏳ Next |
| 3 | Desktop Agent: Claude adapter, session manager, power manager, reconnect | ⏸ Planned |
| 4 | Dashboard: responsive UI, realtime, device management, live session | ⏸ Planned |
| 5 | Integration: prompt forwarding and live logs end to end | ⏸ Planned |
| 6 | Deployment: Railway, Supabase, Upstash, Vercel | ⏸ Planned |
| 7 | Testing, performance, security review | ⏸ Planned |

**What works right now:** both Go binaries build and run on Windows, macOS, and
Linux; configuration resolves through CLI → env → file → defaults with validation;
structured JSON logging with secret redaction; the WebSocket protocol is fully
defined and tested; `beuvian-agent -detect` reports which coding agents are
installed on your machine.

**What does not work yet:** there is no HTTP server, no database, no dashboard, and
no adapter can actually drive a coding agent. Those are Phases 2–4.

---

## Quick start

Requires **Go 1.26+**. Docker is optional (for Postgres and Redis locally).

```bash
git clone https://github.com/bhuvan0808/beuviancode.git
cd beuviancode

# Every module compiles independently, and all tests run with the race detector.
make check

# What coding agents are installed here?
make detect

# Validate configuration for both binaries.
make config-check
```

No configuration file is needed to start: the defaults point at a local backend and
a fresh clone runs immediately. `make help` lists every task.

### Running the pieces

```bash
# Postgres + Redis only (stand-ins for Supabase and Upstash), backend from source
make infra
make run-backend

# Or the whole stack in containers
make up
make logs
```

### Building the agent

```bash
make build-agent           # host platform
make build-agent-all       # all six release targets, with SHA256SUMS
```

Windows users without `make`:

```powershell
./scripts/build-agent.ps1 -Target All
```

---

## How it is put together

```
┌──────────────────┐         WSS          ┌──────────────────┐
│  Desktop Agent   │◀────────────────────▶│     Backend      │
│      (Go)        │   versioned protocol │   (Go + Fiber)   │
│                  │                      │                  │
│ supervises your  │                      │  ┌────────────┐  │
│  coding agent    │                      │  │  Postgres  │  │
│  ▼               │                      │  │   Redis    │  │
│ Claude Code      │                      │  └────────────┘  │
└──────────────────┘                      └────────┬─────────┘
                                                   │ WSS + REST
                                          ┌────────▼─────────┐
                                          │    Dashboard     │
                                          │    (Next.js)     │
                                          │  phone / laptop  │
                                          └──────────────────┘
```

Three Go modules, each compiling independently:

| Module | Purpose |
| ------ | ------- |
| [`shared/`](shared/) | The wire protocol and cross-cutting libraries. **Zero third-party dependencies** — enforced in CI. |
| [`backend/`](backend/) | API and WebSocket gateway, in Clean Architecture layers. |
| [`agent/`](agent/) | The Desktop Agent, with a pluggable coding-agent adapter. |
| [`dashboard/`](dashboard/) | Next.js dashboard (Phase 4). |

The interesting decisions, and why they were made, are in
[`docs/adr/`](docs/adr/). The short version:

- **`shared/` is dependency-free** so neither binary inherits the other's
  dependency graph — the agent must not pull in Fiber, the backend must not pull in
  OS power syscalls. CI fails if a `require` appears.
- **Three modules, not one**, so each compiles standalone. CI builds every module
  with `GOWORK=off` to prove it.
- **The adapter interface is the extension point.** Claude Code ships in the MVP;
  Codex, Gemini, Aider, and OpenHands are registered placeholders that are already
  detected on your PATH. Adding a real one is one file plus one `Register` call.
- **The domain layer imports no drivers**, so Supabase or Upstash can be swapped
  without touching a business rule.

---

## Roadmap

The operating system grows along two axes: **more agents** and **more ways to talk
to it**.

**Agent support (driver layer).** The adapter registry is built; real drivers land
phase by phase:

- Claude Code (Phase 3, MVP) → Codex → Gemini CLI → Aider → OpenHands
- The future belongs to open tools: if your agent can be scripted, it can be driven.

**Surfaces (transport layer).** Remote control should come from wherever you are:

| Surface | Channel | Status |
| ------- | ------- | ------ |
| Dashboard | Web (Next.js), WebSocket + REST | ⏳ Phase 4 |
| Telegram | Bot API — prompts in, notifications out | 🚀 Future scope |
| WhatsApp | WhatsApp Business / Cloud API | 🚀 Future scope |
| Voice | Call integration: "tell the agent to fix the tests" | 🚀 Future scope |

Each new surface is an edge transport plugged into the same backend, event stream,
and prompt queue — an architecture decision made in Phase 1 so these can ship
without restructuring. The full product specification lives in
[PROJECT.md](PROJECT.md).

---

## Documentation

| | |
| --- | --- |
| [Architecture Guide](docs/ARCHITECTURE.md) | Layers, boundaries, and data flow |
| [Developer Guide](docs/DEVELOPER_GUIDE.md) | Setup, workflow, testing, conventions |
| [WebSocket Protocol](docs/WEBSOCKET_PROTOCOL.md) | The versioned realtime contract |
| [API Documentation](docs/API.md) | REST surface |
| [Database Documentation](docs/DATABASE.md) | Schema and design reasoning |
| [Deployment Guide](docs/DEPLOYMENT.md) | Railway, Supabase, Upstash, Vercel |
| [Folder Structure](docs/FOLDER_STRUCTURE.md) | What lives where, and why |
| [Troubleshooting](docs/TROUBLESHOOTING.md) | Common problems |
| [Contributing](CONTRIBUTING.md) | How to work on this |
| [Decision Records](docs/adr/) | Architectural decisions with rationale |
| [PROJECT.md](PROJECT.md) | The product specification — the source of truth |

---

## Security

Beuvian holds credentials that grant control over a developer's machine, so the
security posture is part of the design rather than a later hardening pass:

- Device tokens are scoped to a single device, separate from dashboard sessions.
- Local agent state is encrypted at rest, keyed through the OS secret store.
- The WebSocket handshake carries a nonce and a bounded timestamp for replay
  protection.
- Secrets are stripped from logs by the logging handler itself, not by convention.
- The backend image is distroless and runs as a non-root user with no shell.
- Production configuration is validated at boot: plaintext origins, insecure
  cookies, short signing secrets, and debug logging are rejected outright.

Found a vulnerability? Please report it privately rather than opening a public
issue.

## Contributing

Contributions are welcome — issues, PRs, new coding-agent adapters, new surfaces,
and honest critique of the design. Read [CONTRIBUTING.md](CONTRIBUTING.md) and run
`make check` before opening a pull request.

## License

[MIT](LICENSE) — free to use, modify, and distribute.
