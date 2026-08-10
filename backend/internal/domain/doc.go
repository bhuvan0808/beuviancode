// Package domain holds Beuvian's business entities and rules.
//
// # Layer contract
//
// This is the innermost layer. It imports NOTHING from the rest of the backend,
// and nothing third-party beyond the standard library and shared/protocol.
//
// That constraint is the whole point of Clean Architecture here, and it buys
// something concrete rather than theoretical: PROJECT.md names Supabase and
// Upstash specifically, and both are replaceable managed services. Because no
// business rule imports a driver, swapping Supabase for plain PostgreSQL, or
// Upstash for a self-hosted Redis, touches only internal/adapter. If a session
// rule imported pgx, that swap would become a rewrite.
//
// The same property makes the layer testable with no infrastructure at all: a
// domain test needs no database, no container, and no network.
//
// # What belongs here
//
//   - Entities: User, Device, Repository, Session, SessionLog, Message,
//     Notification, PromptQueueItem, AgentStatus, UserSettings, OAuthAccount,
//     RefreshToken — mirroring the tables PROJECT.md specifies.
//   - Value objects and enums with their invariants.
//   - Rules that hold regardless of storage: which state transitions are legal,
//     whether a device may accept a prompt, when a session counts as stale.
//   - Domain errors, so a use case can distinguish "not found" from "forbidden"
//     without inspecting a driver error.
//
// # What does NOT belong here
//
//   - SQL, struct tags for a driver, or any persistence concern.
//   - HTTP or WebSocket types. An entity that knows about a status code has
//     leaked a transport detail into the business model.
//   - Anything with a network call.
//
// Populated in Phase 2 alongside the database schema.
package domain
