// Package port declares the interfaces the application layer depends on.
//
// # Why the interfaces live here and not beside their implementations
//
// This is the dependency inversion that makes the architecture work. The
// application layer needs to persist a session; it must not import the Postgres
// adapter to do so. So the interface is declared here, in a package the
// application layer owns, and the adapter is written to satisfy it.
//
// The dependency arrow therefore points inward at every layer boundary:
//
//	adapter ---> port <--- app ---> domain
//
// The adapter depends on the interface; the interface depends on nothing. This is
// what lets Phase 2 write use cases and their tests before any real database code
// exists, and what lets a use-case test substitute an in-memory fake with no
// mocking framework.
//
// Go's implicit interface satisfaction is what makes this cheap: an adapter needs
// no reference to this package at all, it simply has the right method set.
//
// # Interface granularity
//
// Interfaces are kept narrow and named for what the caller needs, not for what
// the storage engine offers. A single sprawling Repository interface with thirty
// methods forces every fake to implement thirty methods to test one, which is how
// tests become expensive enough that people stop writing them. Prefer
// SessionReader and SessionWriter over SessionStore.
//
// # Expected interfaces (Phase 2)
//
//	Persistence : UserStore, DeviceStore, RepositoryStore, SessionStore,
//	              SessionLogStore, PromptQueueStore, NotificationStore,
//	              RefreshTokenStore, AuditLogger
//	Cache/queue : PresenceTracker, PromptDispatcher, RateLimiter, DistributedLock
//	Realtime    : EventPublisher, ConnectionRegistry
//	Identity    : OAuthProvider, TokenIssuer, TokenVerifier
//	Time/IDs    : Clock, IDGenerator — injected rather than called directly, so
//	              time-dependent rules (token expiry, stale sessions) are testable
//	              without sleeping.
//
// # Extension points (interfaces only, per PROJECT.md)
//
// PROJECT.md requires the future integrations to be designed but not built.
// Declaring them as interfaces here is exactly that: NotificationChannel is
// satisfied in the MVP only by the dashboard channel, while WhatsApp, Telegram,
// Slack, Discord, and push become additional implementations with no change to
// the notification use case. Likewise BillingProvider and OrganizationStore.
package port
