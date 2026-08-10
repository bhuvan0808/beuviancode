// Package transport maintains the agent's authenticated WebSocket to the backend.
//
// # Responsibilities (Phase 3)
//
//   - Dial the gateway, perform the AUTH handshake, and wait for ACK.
//   - Send PING every protocol.HeartbeatInterval and treat a missing PONG within
//     HeartbeatTimeout as a dead connection.
//   - Reconnect with exponential backoff and jitter from shared/retry, using
//     ReconnectPolicy so attempts never exhaust.
//   - Buffer outbound frames while disconnected, bounded by
//     queue.max_outbound_events, dropping oldest and marking batches truncated.
//   - Refresh the device token over REST before it expires.
//   - Deduplicate redelivered inbound frames by envelope ID.
//
// # Reconnection is the feature, not an afterthought
//
// The agent runs on a laptop that sleeps, changes networks, and loses Wi-Fi. A
// disconnect is the normal case, not an error, and the reconnect loop is therefore
// unbounded on purpose: a machine closed for the weekend must reconnect on Monday
// rather than having given up on Friday.
//
// The one thing that must NOT retry forever is a rejected credential. An agent with
// a revoked token retrying every 30 seconds becomes a denial-of-service against our
// own gateway, multiplied by every installed agent. protocol.ErrorCode.Retryable
// draws that line, and it defaults to non-retryable precisely so a new error code
// added carelessly fails safe.
//
// # Jitter is load-bearing
//
// When the backend restarts, every connected agent reconnects at once. Without
// jitter their retries stay synchronised and the herd keeps the gateway down —
// the outage becomes self-sustaining. ReconnectPolicy sets 30% jitter for this
// reason, not for elegance.
//
// Populated in Phase 3.
package transport
