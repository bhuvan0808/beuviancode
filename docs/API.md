# API Documentation

**Status: design. Implemented in Phase 2.**

REST surface of the Beuvian backend. The realtime half is documented separately in
[WEBSOCKET_PROTOCOL.md](WEBSOCKET_PROTOCOL.md).

```
https://api.beuvian.example/v1     production
http://localhost:8080/v1           local
```

## Conventions

| | |
| --- | --- |
| Versioning | Path prefix `/v1`. A breaking change means `/v2`, with `/v1` kept alive — dashboards cache and agents lag |
| Content type | `application/json; charset=utf-8` |
| Timestamps | RFC 3339 with offset, always UTC (`2026-08-05T12:34:56.789Z`) |
| IDs | Prefixed ULIDs (`dev_01J9Z…`) |
| Auth | `Authorization: Bearer <access token>` |
| Request ID | Echo `X-Request-ID`, or one is generated. Include it in bug reports |

### Errors

One shape for every failure, so a client needs one parser:

```json
{
  "error": {
    "code": "device_not_found",
    "message": "no device with that id belongs to this user",
    "request_id": "req_01J9Z3K7QF8XKM2N4P6R8T0VWY",
    "details": {}
  }
}
```

`code` is stable and machine-readable; `message` is for humans and may change.
Clients must branch on `code`, never on `message`.

| Status | When |
| --- | --- |
| 400 | Malformed body or parameters |
| 401 | Missing, invalid, or expired access token |
| 403 | Authenticated but not permitted |
| 404 | Not found, **or** found but not owned by the caller |
| 409 | Conflict with current state |
| 422 | Well-formed but semantically invalid |
| 429 | Rate limited — `Retry-After` is set |
| 500 | Server fault |

404 rather than 403 for another user's resource is deliberate: a 403 confirms the ID
exists, which lets an attacker enumerate valid IDs.

### Rate limits

Per identity (user ID when authenticated, else client IP), enforced in Redis.
Defaults: 120 requests/minute, and 10/minute on auth endpoints — the ones worth
brute-forcing get the tighter budget.

```
X-RateLimit-Limit: 120
X-RateLimit-Remaining: 118
X-RateLimit-Reset: 1786000000
```

### Pagination

Cursor-based, not offset-based. Offsets skip or duplicate rows when data is inserted
between pages, which for an append-only log is guaranteed rather than unlikely.

```
GET /v1/sessions?limit=50&cursor=ses_01J9Z3K7QF8XKM2N4P6R8T0VWY
```

```json
{ "data": [], "next_cursor": "ses_01J9Z…", "has_more": true }
```

---

## Authentication

Beuvian issues two independent credential families. A leaked device token must not
grant dashboard access, and a leaked browser session must not silently control every
machine — so they never overlap.

| | Access token | Refresh token | Device token |
| --- | --- | --- | --- |
| Bearer | dashboard | dashboard | agent |
| Lifetime | 15m | 30d | 90d |
| Storage | memory | `HttpOnly` `Secure` `SameSite=Lax` cookie | encrypted agent state |
| Revocable | no (bounded by expiry) | yes | yes |

Access tokens are short-lived precisely *because* they are not revocable without a
database lookup on every request; expiry bounds the blast radius instead.

### `GET /v1/auth/github`

Begins the OAuth flow. Redirects to GitHub with a `state` parameter stored in Redis
under `auth.state_ttl` (10m) — the CSRF defence for the authorization code flow.

### `GET /v1/auth/github/callback`

GitHub redirects here. The backend validates `state`, exchanges the code, upserts the
user, sets the refresh cookie, and redirects to `auth.dashboard_url`.

The callback URL must match the GitHub OAuth app **exactly**, including scheme and
trailing path.

### `POST /v1/auth/refresh`

Reads the refresh cookie, rotates it, returns a new access token.

```json
{ "access_token": "eyJ…", "expires_in": 900, "token_type": "Bearer" }
```

**Reuse detection:** each refresh marks the old token used and issues a new one in
the same family. Presenting an already-used token revokes the whole family — that
pattern means the token was stolen and both parties now hold it, so ending every
session is the correct response.

### `POST /v1/auth/logout`

Revokes the refresh token family and clears the cookie.

### `GET /v1/auth/me`

```json
{
  "id": "usr_01J9Z…",
  "github_login": "octocat",
  "name": "The Octocat",
  "avatar_url": "https://…",
  "created_at": "2026-08-05T12:00:00Z"
}
```

---

## Devices

### `POST /v1/devices/register`

Called by the agent once, on first run. Requires a **user** access token; returns a
**device** token. This is the one place the two families touch, and only to bootstrap.

```json
{ "name": "BODDU-DESKTOP", "platform": "windows/amd64",
  "agent_version": "v0.1.0", "capabilities": ["claude"] }
```

```json
{ "device": { "id": "dev_01J9Z…", "name": "BODDU-DESKTOP" },
  "device_token": "eyJ…", "expires_at": "2026-11-03T12:00:00Z" }
```

The token is returned **once** and never retrievable again — it is stored hashed. A
lost token means re-registering, which is the correct trade for a credential that
controls a developer's machine.

### `GET /v1/devices`

```json
{ "data": [{
  "id": "dev_01J9Z…", "name": "BODDU-DESKTOP", "platform": "windows/amd64",
  "agent_version": "v0.1.0", "capabilities": ["claude"],
  "online": true, "last_seen_at": "2026-08-05T12:34:50Z",
  "status": { "state": "running", "cpu_percent": 12.4,
              "memory_bytes": 284164096, "queued_prompts": 1 }
}] }
```

`online` comes from Redis presence, not from `last_seen_at` arithmetic: presence is
authoritative and the timestamp is for display.

### `GET /v1/devices/{id}` · `PATCH /v1/devices/{id}` · `DELETE /v1/devices/{id}`

### `POST /v1/devices/{id}/revoke`

Invalidates the device token and closes its socket. Distinct from `DELETE`: revoking
a compromised machine's access is not the same as removing it from the user's list,
and conflating them makes "revoke but keep the history" impossible.

---

## Repositories

`GET /v1/repositories` · `POST /v1/repositories` · `GET|PATCH|DELETE /v1/repositories/{id}`

### `GET /v1/repositories/github`

Lists the user's GitHub repositories, using the stored OAuth token. Read-only
metadata. Cached briefly in Redis — GitHub's rate limit is per user and a dashboard
refresh loop would exhaust it.

---

## Sessions

### `POST /v1/sessions`

Starts a coding session on a device.

```json
{ "device_id": "dev_01J9Z…", "repository_id": "rep_01J9Z…",
  "adapter": "claude", "working_directory": "C:\\src\\beuvian",
  "initial_prompt": "Implement the auth middleware." }
```

`409` if a session is already running on that device. `422` if the device's
`capabilities` do not include the requested adapter — checked here rather than
failing later on the device, so the user learns immediately.

### `GET /v1/sessions`

Filter by `device_id`, `state`, `active=true`. Cursor paginated.

### `GET /v1/sessions/{id}`

### `POST /v1/sessions/{id}/stop`

Requests a graceful stop; escalates to a kill if the agent does not comply.

### `GET /v1/sessions/{id}/logs`

```
GET /v1/sessions/{id}/logs?after_seq=1200&limit=500
```

```json
{ "data": [{ "seq": 1201, "stream": "stdout",
              "content": "Reading src/auth.go",
              "truncated": false, "at": "2026-08-05T12:34:56.789Z" }],
  "next_seq": 1202, "has_more": true }
```

Paginated by `seq`, not by timestamp: timestamps collide under load and are not
guaranteed monotonic across a clock adjustment, so paging by them can skip lines.

Live tailing uses the WebSocket. This endpoint is for history and for a client that
connects mid-session.

---

## Prompts

### `POST /v1/prompts`

**The core operation.** This is what the phone calls.

```json
{ "device_id": "dev_01J9Z…", "session_id": "ses_01J9Z…",
  "text": "Now implement authentication." }
```

`202 Accepted`:

```json
{ "id": "prm_01J9Z…", "status": "pending",
  "enqueued_at": "2026-08-05T12:35:00Z",
  "correlation_id": "cor_01J9Z…" }
```

`202`, not `200`, and the distinction is real: the prompt is durably queued but not
yet delivered. **The row is committed to PostgreSQL before this response is sent**,
then published to Redis for immediate dispatch. If Redis loses the message the prompt
is still delivered on the next agent reconnect — which is why an offline device is
not an error here. The user gets `202` regardless, and the dashboard shows the prompt
as queued.

Keep the returned `correlation_id`: it traces the prompt through the backend, the
socket, the agent, and the resulting log lines.

### `GET /v1/prompts` · `DELETE /v1/prompts/{id}`

`DELETE` cancels a prompt that has not yet been delivered; `409` once it has, because
it cannot be un-injected.

---

## Notifications

`GET /v1/notifications` (filter `unread=true`) ·
`POST /v1/notifications/{id}/read` · `POST /v1/notifications/read-all`

## Settings

`GET /v1/settings` · `PATCH /v1/settings`

## Health

### `GET /health`

Liveness. No auth, no dependency checks — it answers "is this process running?". If
it checked the database, a brief database blip would make the orchestrator kill
otherwise-healthy instances and turn a partial degradation into an outage.

```json
{ "status": "ok", "version": "v0.1.0", "commit": "a1b2c3d" }
```

### `GET /health/ready`

Readiness. Verifies dependencies and is what a load balancer should gate traffic on.

```json
{ "status": "ok",
  "checks": { "database": "ok", "redis": "ok" },
  "version": "v0.1.0" }
```

Returns `503` when a required dependency is down. Redis being unavailable is
reported as `degraded` rather than `down` when `redis.required` is false, because the
backend genuinely still works — prompts persist and deliver on reconnect.

---

## Designed but not implemented

Per PROJECT.md these are extension points only:

| Feature | Shape |
| --- | --- |
| Public API + SDK | The same `/v1` surface with API-key auth. Use cases already carry no transport dependency, so this adds no business logic |
| Organisations | `/v1/organizations/*`; resources already carry a nullable `organization_id` |
| Billing | `/v1/billing/*` behind `port.BillingProvider` |
| Push, WhatsApp, Telegram, Slack, Discord | Additional `port.NotificationChannel` implementations. `notifications.kind` is machine-readable so routing needs no prose parsing |
