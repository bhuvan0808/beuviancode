// Package app contains the application's use cases.
//
// # Layer contract
//
// May import domain and port. Must NOT import adapter, Fiber, pgx, or any Redis
// client. A use case that imports a driver cannot be tested without that driver
// running, which is the cost this boundary exists to avoid.
//
// # What a use case is
//
// One user-visible operation, with its orchestration and authorisation, and no
// transport concerns. "Forward a prompt to a device" is a use case: it validates
// the device belongs to the caller, persists the prompt to the queue, publishes a
// dispatch event, and records an audit entry. It knows nothing about HTTP status
// codes or JSON.
//
// Keeping transport out is what lets one use case serve several entry points. The
// same ForwardPrompt runs from the dashboard's REST call today and from the
// planned public REST API and WhatsApp integration later, with no duplication —
// which is what PROJECT.md means by "no duplicated business logic".
//
// # Dependency injection
//
// Use cases receive their collaborators through a constructor and hold them as
// port interfaces. No package-level state, no service locator, no init(). A
// constructor makes dependencies visible in the signature, so an over-coupled use
// case is obvious at a glance rather than hidden behind global lookups.
//
//	type PromptService struct {
//	    devices    port.DeviceStore
//	    queue      port.PromptQueueStore
//	    dispatcher port.PromptDispatcher
//	    audit      port.AuditLogger
//	    clock      port.Clock
//	    log        *slog.Logger
//	}
//
// # Errors
//
// Use cases return domain errors, never transport errors. Mapping a domain error
// to an HTTP status is the adapter's job; doing it here would mean a use case
// reused over WebSocket returned an HTTP concept to a caller that has no statuses.
//
// # Expected services (Phase 2)
//
//	AuthService         GitHub OAuth, token issuance, refresh rotation
//	DeviceService       registration, presence, revocation
//	RepositoryService   repository management
//	SessionService      lifecycle, logs, history
//	PromptService       queueing, forwarding, acknowledgement
//	NotificationService fan-out over NotificationChannel implementations
//	SettingsService     user preferences
package app
