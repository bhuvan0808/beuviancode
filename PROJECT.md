# Beuvian

**Control AI Coding Agents From Anywhere.**

> This document is the **single source of truth** for the project. If any
> implementation decision conflicts with these requirements, stop and explain the
> conflict before writing code. Resolved ambiguities are recorded as ADRs in
> [`docs/adr/`](docs/adr/).

---

## Role

Lead Software Architect, Principal Software Engineer, Senior Go Engineer, DevOps
Engineer, Security Engineer, Product Designer, QA Engineer, Technical Writer.

Build a complete **production-ready SaaS**. Not a prototype. Not a hackathon
project. Software that could realistically be launched to paying customers.

Everything must be: production-ready, Clean Architecture, SOLID, modular,
extensible, secure, cross-platform, well documented, scalable, easily testable.

Every architectural decision must be explained. Never sacrifice maintainability
for short-term speed.

---

## Product overview

Beuvian lets developers remotely control AI coding agents from anywhere.

1. Developer starts Claude Code.
2. Claude works for 45 minutes and completes the task.
3. Beuvian immediately updates the dashboard.
4. Developer opens Beuvian on a phone and types *"Now implement authentication."*
5. The Desktop Agent injects the prompt.
6. Claude immediately continues.

The developer never touches the laptop.

---

## What Beuvian is NOT

- Beuvian is **not** an AI.
- Beuvian **never** calls Anthropic APIs.
- Beuvian **never** calls OpenAI APIs.
- Beuvian **never** replaces Claude Code.

Users continue paying for their own coding agents. Beuvian provides only: remote
control, session management, communication, notifications, live monitoring, and
prompt forwarding.

---

## MVP scope

**Must include**

- Windows Desktop Agent
- Backend API
- Live Web Dashboard
- GitHub Login
- Claude Code Support
- Prompt Queue
- Live Logs
- Realtime Status
- Session Management
- Mobile Responsive Dashboard

**Must NOT implement** (design extension points only)

- WhatsApp, Telegram, Discord, Voice Calls, Billing, Organizations

---

## Technology stack

| Concern            | Choice                                  |
| ------------------ | --------------------------------------- |
| Desktop Agent      | Go                                      |
| Backend            | Go + Fiber                              |
| Database           | Supabase PostgreSQL                     |
| Cache / queue      | Upstash Redis                           |
| Dashboard          | Next.js, React, TypeScript, TailwindCSS |
| Authentication     | GitHub OAuth                            |
| Realtime           | WebSockets                              |
| Backend deployment | Railway                                 |
| Dashboard hosting  | Vercel                                  |
| Storage            | Supabase Storage                        |
| Containerization   | Docker (backend only)                   |
| CI/CD              | GitHub Actions                          |
| Version control    | Git                                     |

---

## Monorepo structure

```
beuvian/
  agent/       Desktop Agent (Go)
  backend/     API + WebSocket gateway (Go)
  dashboard/   Next.js dashboard
  shared/      Wire protocol + cross-cutting Go libraries
  docs/        Documentation
  docker/      Container definitions
  infra/       Deployment manifests
  scripts/     Build and dev scripts
  .github/     CI/CD workflows
```

Every module must compile independently.

---

## Desktop Agent

The core of the product. Responsibilities:

- Launch Claude Code
- Detect Claude installation
- Monitor Claude process
- Read stdout / stderr
- Detect when Claude is waiting for input
- Detect task completion
- Maintain a secure WebSocket
- Authenticate with backend
- Reconnect automatically
- Queue prompts while offline
- Send logs, status updates, heartbeat
- Persist local configuration
- Prevent computer sleep while active
- Recover after crashes
- Support a future auto-update architecture

---

## Coding-agent adapter

A pluggable adapter interface:

`Start()` · `Stop()` · `Status()` · `SendPrompt()` · `ReadOutput()` ·
`CurrentTask()` · `Repository()` · `WorkingDirectory()` · `ExitCode()`

Implement `ClaudeAdapter`. Create placeholder adapters for Codex CLI, Gemini CLI,
Aider, and OpenHands. Future adapters must require minimal code changes.

---

## Backend responsibilities

Authentication · Device Registration · Repository Management · Prompt Queue ·
Realtime Events · Live Logs · Session Management · Agent Management ·
Notifications · WebSocket Gateway · Settings · Audit Logging ·
Future Billing · Future Teams · Future Analytics

---

## Database

A production PostgreSQL schema with these tables:

`users` · `devices` · `repositories` · `sessions` · `session_logs` · `messages` ·
`notifications` · `prompt_queue` · `agent_status` · `user_settings` ·
`oauth_accounts` · `refresh_tokens`

Include indexes, foreign keys, constraints, migrations, seed scripts, soft
deletes where appropriate, `created_at`, and `updated_at`.

---

## Redis

Use Upstash Redis **only** for: prompt queue (hot dispatch), presence, heartbeat,
online devices, rate limiting, distributed locks, temporary cache.

**Never store permanent business data in Redis.**

---

## WebSocket

A persistent connection with a versioned protocol. Messages:

`AUTH` · `PING` · `PONG` · `STATUS` · `LOG` · `PROMPT` · `TASK_COMPLETE` ·
`TASK_WAITING` · `DEVICE_ONLINE` · `DEVICE_OFFLINE` · `ERROR` · `ACK` ·
`NOTIFICATION`

Heartbeat every 30 seconds. Automatic reconnection with exponential backoff.
Protocol documentation required — see [`docs/WEBSOCKET_PROTOCOL.md`](docs/WEBSOCKET_PROTOCOL.md).

---

## Dashboard

**Pages:** Login · Dashboard · Devices · Repositories · Live Session ·
Notifications · Session History · Settings · Device Details

Must be fully responsive and work perfectly on phones.

**Must display:** current repository, connected devices, offline devices, current
AI agent, running session, current task, connection status, CPU usage, memory
usage, elapsed runtime, latest logs, queued prompts.

**Live Session:** realtime terminal, auto scroll, prompt textbox, send prompt,
reconnect, stop session, live status. Future placeholders for git diff,
screenshot preview, and deployment status.

---

## User flow

```
Install Desktop Agent → Login with GitHub → Desktop Agent connects
  → Start Claude Code → Agent detects Claude → Claude begins work
  → Backend receives events → Dashboard updates live
  → User opens phone → Types new prompt → Backend forwards prompt
  → Desktop Agent injects prompt → Claude continues
```

---

## Power management

Interface: `PreventSleep()` · `AllowSleep()` · `Status()`

Implement for Windows, macOS, and Linux. Keep the system awake **only** while
Beuvian controls an active coding session.

---

## Configuration

Support `config.yaml`, environment variables, and CLI flags.

Priority: **CLI → Environment → Config file → Defaults**

---

## Logging

Structured JSON logs at DEBUG / INFO / WARN / ERROR, including timestamp,
session ID, device ID, correlation ID, and request ID.

---

## Security

HTTPS · WSS · JWT · Refresh Tokens · Encrypted Local Config · Device IDs ·
Replay Protection · Rate Limiting · Secure Cookies · Input Validation · CORS

---

## Error handling

Graceful recovery, automatic reconnect, retry failed requests, offline queue,
exponential backoff. Never crash on temporary failures.

---

## Testing

Unit tests · Integration tests · Authentication tests · WebSocket tests ·
Repository tests · Prompt queue tests · Agent tests

---

## Deployment

- **Backend:** Dockerfile, docker-compose, Railway deployment, GitHub Actions
- **Dashboard:** Vercel deployment
- **Agent:** cross-platform build scripts (Windows, macOS, Linux) + release workflow

---

## Documentation

README · Architecture Guide · Developer Guide · Deployment Guide ·
API Documentation · Database Documentation · Folder Structure Guide ·
Contribution Guide · Troubleshooting Guide

---

## Future extensions (interfaces only — do not implement)

WhatsApp · Telegram · Slack · Discord · Voice Calls · Push Notifications ·
Organizations · Billing · Subscriptions · Cloud-hosted Agents · REST API ·
Public SDK · Plugin Marketplace

---

## Development rules

- **Never** generate the entire project in one response.
- Work **phase by phase**.
- Every phase must compile successfully before moving on.

At the end of every phase: explain what was built, explain architecture
decisions, explain tradeoffs, list files created, provide commands to run
locally, provide test instructions, list remaining work, **then wait for
approval**.

---

## Development phases

| Phase | Contents                                                                                     | Status |
| ----- | -------------------------------------------------------------------------------------------- | ------ |
| 1     | Project planning, architecture, folder structure, configuration, Docker, CI/CD, documentation | ✅ done |
| 2     | Backend foundation: Fiber, authentication, database, Redis, WebSockets, logging, configuration | ✅ done |
| 3     | Desktop Agent: Claude adapter, session manager, power manager, reconnect logic                | ✅ done |
| 4     | Dashboard: authentication, responsive UI, realtime, device management, live session           | ⏳ next |
| 5     | Integration: backend, agent, dashboard, realtime, prompt forwarding, live logs                | ⏸      |
| 6     | Deployment: Railway, Supabase, Upstash, Vercel, GitHub Actions, Docker                        | ⏸      |
| 7     | Testing, bug fixes, performance, security review, documentation                               | ⏸      |

---

## Coding standards

- Use idiomatic Go.
- Use dependency injection where appropriate.
- Prefer interfaces over tightly coupled implementations.
- Keep packages cohesive and small.
- No duplicated business logic.
- Avoid global mutable state.
- Write meaningful commit messages.
- Add comments only where they improve understanding.
- Favor composition over inheritance.
- Follow consistent naming conventions.
- Optimize for readability and long-term maintenance.

---

## Final goal

A developer can install the desktop agent, sign in with GitHub, connect to the
Beuvian backend, start Claude Code locally, view Claude's status and logs from a
phone, send new prompts remotely, have the desktop agent deliver those prompts to
Claude Code, and continue long-running AI coding sessions without returning to
the laptop.
