// Package adapter holds the outermost layer: everything that talks to the world.
//
// # Layer contract
//
// Adapters may import domain, port, and app. Nothing imports adapter except
// cmd/server, which assembles the process.
//
// This is where all the messy specifics are deliberately concentrated: SQL
// dialects, driver types, Fiber handlers, WebSocket upgrades, HTTP status codes,
// GitHub's OAuth quirks, Redis commands. Anything that would change if we replaced
// a vendor lives here, and nowhere else.
//
// # Subpackages
//
//	http/      Fiber routing, handlers, middleware, request/response DTOs.
//	           Translates HTTP to use-case calls and domain errors to statuses.
//	postgres/  Supabase PostgreSQL implementations of the port store interfaces,
//	           plus migrations wiring.
//	redis/     Upstash Redis: presence, heartbeat, prompt dispatch, rate limiting,
//	           distributed locks, ephemeral cache. Per PROJECT.md, never the
//	           system of record for business data.
//	ws/        WebSocket gateway: the protocol handshake, per-connection
//	           read/write pumps, the connection hub, and fan-out.
//	oauth/     GitHub OAuth client.
//
// # DTOs are not domain entities
//
// Each adapter defines its own request and response types rather than serialising
// domain entities directly. This looks like duplication and is not: the wire
// format is a public contract with its own compatibility requirements, and
// marshalling entities straight out means any internal rename becomes a breaking
// API change, while any new internal field is published by accident. The explicit
// mapping is the seam that keeps refactoring safe.
//
// # Error translation
//
// Adapters map domain errors to their transport's vocabulary — domain.ErrNotFound
// to 404 in http/, to a protocol ERROR frame in ws/. The mapping is defined once
// per adapter so a new handler cannot invent its own status conventions.
//
// Populated in Phase 2.
package adapter
