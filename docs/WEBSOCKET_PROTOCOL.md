# WebSocket Protocol

**Version 1** · Minimum supported version 1

The realtime contract between the Beuvian Desktop Agent, the backend gateway, and
the dashboard.

The authoritative definition is Go source in
[`shared/protocol`](../shared/protocol/) — both peers compile against those exact
types, so a producer/consumer mismatch is a build failure rather than a production
bug. **This document must be updated in the same commit as any change there.**

---

## 1. Endpoint and connection

```
wss://api.beuvian.example/v1/ws     production
ws://localhost:8080/v1/ws           local development
```

| Property | Value | Source |
| --- | --- | --- |
| Subprotocol | `beuvian.v1` | |
| Max frame size | 1 MiB | `protocol.MaxMessageBytes` |
| Heartbeat interval | 30s | `protocol.HeartbeatInterval` |
| Heartbeat timeout | 75s | `protocol.HeartbeatTimeout` |
| Max clock skew | 2m | `protocol.MaxClockSkew` |
| Handshake timeout | 10s | `websocket.handshake_timeout` |

Heartbeat timeout is 2.5× the interval, not 1×, so a single dropped frame does not
tear down an otherwise healthy connection. A test asserts the relationship holds.

---

## 2. Envelope

Every frame is a JSON object with this outer shape:

```json
{
  "v": 1,
  "id": "msg_01J9Z3K7QF8XKM2N4P6R8T0VWY",
  "type": "STATUS",
  "ts": "2026-08-05T12:34:56.789Z",
  "seq": 42,
  "device_id": "dev_01J9Z3K7QF8XKM2N4P6R8T0VWY",
  "session_id": "ses_01J9Z3K7QF8XKM2N4P6R8T0VWY",
  "correlation_id": "cor_01J9Z3K7QF8XKM2N4P6R8T0VWY",
  "payload": { }
}
```

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `v` | int | yes | Protocol version |
| `id` | string | yes | Unique message ID; correlates `ACK` and deduplicates redelivery |
| `type` | string | yes | One of the 13 types below |
| `ts` | RFC3339 | yes | Sender's UTC timestamp |
| `seq` | uint64 | no | Per-connection monotonic counter; lets the receiver detect gaps and order log lines |
| `device_id` | string | no | Originating device |
| `session_id` | string | no | Owning session |
| `correlation_id` | string | no | Spans the whole causal chain across processes |
| `payload` | object | varies | Type-specific body |

`payload` is held as raw JSON at the transport layer so a frame can be routed and
validated without knowing its concrete type, and an unknown type can be logged and
skipped rather than failing the connection.

### Correlation IDs

A correlation ID follows one user action end to end: dashboard click → HTTP request
→ Redis enqueue → WebSocket delivery → agent injection → the resulting log lines.
One `grep` across three processes reconstructs the whole story. This is the single
most useful field when diagnosing "I sent a prompt and nothing happened".

---

## 3. Message types

The set is **closed** at 13 types.

| Type | Direction | Purpose |
| --- | --- | --- |
| `AUTH` | agent → backend | Authenticate the connection |
| `ACK` | both | Universal positive acknowledgement |
| `ERROR` | both | Failure report |
| `PING` | both | Heartbeat probe |
| `PONG` | both | Heartbeat reply |
| `STATUS` | agent → backend | Coding-agent state snapshot |
| `LOG` | agent → backend | Batched output lines |
| `TASK_WAITING` | agent → backend | Blocked on human input |
| `TASK_COMPLETE` | agent → backend | Work finished |
| `PROMPT` | backend → agent | A prompt to inject |
| `DEVICE_ONLINE` | backend → dashboard | Device connected |
| `DEVICE_OFFLINE` | backend → dashboard | Device disconnected |
| `NOTIFICATION` | backend → dashboard | User-facing notification |

### There is no `AUTH_OK`

A successful `AUTH` is answered with `ACK` carrying the AUTH envelope's `id`; a
failure with `ERROR`. Reusing `ACK` keeps the type set minimal and gives every
request-shaped message one uniform reply shape, so a client needs one code path for
"did it land?" rather than one per message type.

---

## 4. Handshake

```
agent                                            backend
  │                                                  │
  │────── WebSocket upgrade ────────────────────────▶│
  │                                                  │
  │────── AUTH { token, device_id, nonce, ... } ────▶│
  │                                                  │  verify token
  │                                                  │  check ts within 2m
  │                                                  │  check nonce unseen
  │◀───── ACK { ack_id, accepted: true } ────────────│
  │                                                  │
  │  connection is now authenticated                 │
  │                                                  │
  │◀───── PROMPT / PING ────────────────────────────▶│
```

The connection is unauthenticated until `ACK` arrives. An agent that sends anything
other than `AUTH` first is disconnected — this keeps unauthenticated sockets from
consuming gateway resources, and bounds the pre-auth attack surface to one message
type.

### `AUTH` payload

```json
{
  "token": "<device access token>",
  "device_id": "dev_01J9Z3K7QF8XKM2N4P6R8T0VWY",
  "nonce": "9F8XKM2N4P6R8T0VWYQF01J9Z3",
  "agent_version": "v0.1.0",
  "platform": "windows/amd64",
  "hostname": "BODDU-DESKTOP",
  "capabilities": ["claude"]
}
```

`token` is a **device** token, not the user's dashboard session token. A leaked
device token must not grant dashboard access, so the two are separate credentials
with separate lifetimes.

`capabilities` lists coding agents actually **installed on that machine**, not those
compiled into the binary. The backend uses it to avoid dispatching a prompt to a
device that cannot service it, and the dashboard uses it to grey out unavailable
adapters. A build supporting five adapters on a machine with one installed reports
one capability.

### Replay protection

Two mechanisms, and both are needed:

1. **Freshness.** `ts` must be within `MaxClockSkew` (2 minutes) of the receiver's
   clock, checked symmetrically — a frame stamped in the *future* is as suspect as an
   old one, since an attacker controls the clock they stamp with.
2. **Nonce.** The backend caches recently seen AUTH nonces for the skew window and
   rejects duplicates.

Freshness alone would leave a 2-minute replay window. The nonce alone would require
an unbounded cache. Together the cache stays small and the window is closed.

---

## 5. Payloads

### `STATUS` — agent → backend

Sent on **every state transition** and at least once per `session.status_interval`
(10s). The periodic resend is what makes the dashboard converge to the truth even
if a transition frame was lost.

```json
{
  "state": "running",
  "adapter": "claude",
  "repository": "beuvian/beuvian",
  "working_directory": "C:\\src\\beuvian",
  "current_task": "implementing authentication",
  "cpu_percent": 12.4,
  "memory_bytes": 284164096,
  "elapsed_seconds": 2731,
  "pid": 18244,
  "queued_prompts": 1
}
```

`elapsed_seconds` is a duration rather than a start timestamp, so the dashboard need
not trust the agent's wall clock.

#### States

| State | Meaning | Active? |
| --- | --- | --- |
| `idle` | No process running | no |
| `starting` | Launching, not yet confirmed ready | **yes** |
| `running` | Actively working | **yes** |
| `waiting_input` | Blocked on a human — triggers notification | **yes** |
| `stopping` | Graceful shutdown in progress | no |
| `stopped` | Exited as instructed | no |
| `crashed` | Exited unexpectedly | no |

"Active" is exactly when sleep is inhibited. Legal transitions:

```
idle          → starting
starting      → running | crashed | stopped
running       → waiting_input | stopping | stopped | crashed
waiting_input → running | stopping | stopped | crashed
stopping      → stopped | crashed
stopped       → starting
crashed       → starting
```

`idle → running` is **illegal**: it would skip process launch and let the dashboard
claim work is happening with no process behind it.

### `LOG` — agent → backend

```json
{
  "stream": "stdout",
  "lines": ["Reading src/auth.go", "Writing src/auth.go"],
  "at": "2026-08-05T12:34:56.789Z",
  "truncated": false
}
```

Lines are **batched**, not one frame per line. A verbose build emits thousands of
lines per second, and one frame each would saturate both the socket and the
database. The agent flushes on a 250ms timer or when the batch fills.

`stream` is `stdout`, `stderr`, or `system`. The `system` stream carries Beuvian's
own commentary ("injected prompt", "reconnected") interleaved into the transcript,
so the dashboard terminal reads as one coherent story rather than requiring the user
to mentally merge two feeds.

`truncated: true` means lines were dropped because the agent's ring buffer
overflowed. Surfacing this is a correctness requirement: a silently truncated
transcript would read as a complete one.

### `TASK_WAITING` — agent → backend

**This is the message the product exists to deliver.** It triggers the "Claude is
waiting for you" notification on the user's phone.

```json
{
  "reason": "awaiting_prompt",
  "question": "Should I also update the tests?",
  "detected_at": "2026-08-05T12:34:56.789Z"
}
```

| `reason` | Meaning |
| --- | --- |
| `awaiting_prompt` | Task finished, agent idle at its input prompt |
| `awaiting_confirmation` | A tool or permission prompt needs a yes/no |
| `awaiting_error_resolution` | Stopped on an error, cannot proceed unattended |

`question` is best-effort and may be absent. The agent must **not** invent one.

### `TASK_COMPLETE` — agent → backend

```json
{
  "task_id": "",
  "exit_code": -1,
  "duration_seconds": 2731,
  "summary": "Implemented JWT middleware and added tests."
}
```

`exit_code` is `-1` when the coding agent is still alive but merely became idle —
Claude Code stays running between tasks, so "finished the task" and "the process
exited" are genuinely different events.

### `PROMPT` — backend → agent

```json
{
  "prompt_id": "prm_01J9Z3K7QF8XKM2N4P6R8T0VWY",
  "text": "Now implement authentication.",
  "enqueued_at": "2026-08-05T12:30:00.000Z",
  "attempt": 1
}
```

The agent echoes `prompt_id` in its `ACK` so the backend can mark the row delivered
exactly once. `enqueued_at` may be much earlier than delivery if the device was
offline. `attempt` starts at 1 and lets the agent recognise a redelivery and avoid
injecting the same prompt twice.

### `ACK` — both

```json
{ "ack_id": "msg_01J9Z3K7QF8XKM2N4P6R8T0VWY", "accepted": true, "reason": "" }
```

`accepted: false` means the message was received but rejected; `reason` explains
why. Received-and-rejected is distinct from never-arrived, and a sender needs to
tell them apart to decide between re-queueing and giving up.

### `ERROR` — both

```json
{ "code": "unauthorized", "message": "device token has expired", "retryable": false }
```

| Code | Retryable | Meaning |
| --- | --- | --- |
| `unauthorized` | no | Token missing, malformed, or expired |
| `forbidden` | no | Valid token, wrong resource |
| `version_unsupported` | no | Protocol version outside the supported range |
| `replay_detected` | no | Nonce reused or timestamp outside the window |
| `malformed` | no | Failed structural or payload validation |
| `device_not_found` | no | Device unknown or revoked |
| `session_not_found` | no | Referenced session does not exist |
| `adapter_unavailable` | no | Requested coding agent not installed or unsupported |
| `rate_limited` | **yes** | Quota exceeded; back off |
| `internal` | **yes** | Unexpected server-side failure |

**Unrecognised codes default to non-retryable.** This default is deliberate and
load-bearing: an agent with a revoked token that retried forever would become a
denial-of-service against our own gateway, multiplied by every installed agent.
Failing closed is the safe direction.

### `PING` / `PONG` — both

```json
{ "nonce": "QF8XKM2N4P6R8T0VWY9F01J9Z3", "sent_at": "2026-08-05T12:34:56.789Z" }
```

`PONG` echoes the `PING` nonce unchanged, so a peer can match a reply to its own
probe and measure round-trip latency rather than guessing.

### `DEVICE_ONLINE` / `DEVICE_OFFLINE` — backend → dashboard

```json
{
  "device_id": "dev_01J9Z3K7QF8XKM2N4P6R8T0VWY",
  "device_name": "BODDU-DESKTOP",
  "at": "2026-08-05T12:34:56.789Z",
  "graceful": true
}
```

`graceful` distinguishes a clean shutdown from a heartbeat timeout, so the dashboard
can say "went offline" rather than "lost connection" — a real difference to a user
trying to work out whether something is broken.

### `NOTIFICATION` — backend → dashboard

```json
{
  "notification_id": "ntf_01J9Z3K7QF8XKM2N4P6R8T0VWY",
  "kind": "task_complete",
  "title": "Claude finished",
  "body": "Implemented JWT middleware and added tests.",
  "severity": "info",
  "created_at": "2026-08-05T12:34:56.789Z"
}
```

`kind` is a stable machine-readable string, not a display label, because the planned
WhatsApp, Telegram, Slack, and push channels must route on it without parsing prose.

---

## 6. Reconnection

```
                    ┌──────────────┐
                    │  connecting  │
                    └──────┬───────┘
                     ┌─────┴─────┐
                fail │           │ success
                     ▼           ▼
             ┌──────────────┐  ┌──────────────┐
             │ backing off  │  │authenticating│
             └──────┬───────┘  └──────┬───────┘
                    │            ┌────┴────┐
                    │       ACK  │         │ ERROR
                    │            ▼         ▼
                    │      ┌───────────┐ ┌─────────────┐
                    └──────┤ connected │ │ retryable?  │
                           └───────────┘ └──┬───────┬──┘
                                        yes │       │ no
                                            ▼       ▼
                                     back off    STOP
```

Policy (`retry.ReconnectPolicy`):

| Parameter | Value | Reason |
| --- | --- | --- |
| Initial delay | 500ms | Recover fast from a momentary blip |
| Multiplier | 1.8 | |
| Max delay | 30s | A recovered backend is noticed promptly |
| Jitter | 30% | Prevents a synchronised thundering herd |
| Max attempts | **unlimited** | A laptop closed for the weekend must reconnect on Monday |

On every successful connection the backoff **must** be reset. Without that, a
long-lived agent accumulates attempts and eventually waits the maximum delay after a
single blip.

### On reconnect

1. Send `AUTH` with a **fresh** nonce. Reusing the previous one is rejected as a
   replay.
2. Send a `STATUS` frame immediately, so the dashboard is correct without waiting
   for the next interval.
3. Flush buffered outbound frames, oldest first, marking batches `truncated` if any
   were dropped.
4. Deduplicate inbound frames by envelope `id` — the backend may redeliver a
   `PROMPT` it never saw acknowledged.

---

## 7. Versioning

| Change | Bump `Version`? |
| --- | --- |
| New optional payload field | no |
| New message type older peers may ignore | no |
| Removing a field | **yes** |
| Renaming a field | **yes** |
| Changing a field's meaning or units | **yes** |

The backend accepts `[MinSupportedVersion, Version]` so an older agent keeps working
after a backend deploy. This is a hard requirement: agents are installed on users'
machines and cannot be upgraded in lockstep with the server.

An envelope outside that range is answered with `ERROR` / `version_unsupported`,
which is **not** retryable — the agent must be upgraded, and retrying would only
generate load.

---

## 8. Implementation checklist

For anyone implementing a peer:

- [ ] Reject frames over 1 MiB before parsing.
- [ ] Validate the envelope before touching the payload.
- [ ] Send `AUTH` first; treat anything before `ACK` as unauthenticated.
- [ ] Use a fresh nonce for every `AUTH`.
- [ ] `PING` every 30s; treat 75s without `PONG` as dead.
- [ ] Echo the `PING` nonce in `PONG`.
- [ ] Reset backoff on every successful connect.
- [ ] Stop retrying on a non-retryable `ERROR`.
- [ ] Deduplicate inbound frames by envelope `id`.
- [ ] Batch `LOG` lines; set `truncated` when you drop any.
- [ ] Bound the outbound queue and drop the connection rather than blocking.
- [ ] Never log the `token` field.
