package port

import (
	"context"
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// PresenceTracker records which devices are currently connected.
//
// Backed by Redis with a TTL keyed to the heartbeat, so a device that dies
// without a clean disconnect expires rather than appearing online forever. That
// self-healing property is why presence lives in Redis and not in a database
// column somebody has to remember to clear.
type PresenceTracker interface {
	// MarkOnline sets presence with a TTL. Refreshed on every heartbeat.
	MarkOnline(ctx context.Context, userID, deviceID string, ttl time.Duration) error
	MarkOffline(ctx context.Context, userID, deviceID string) error
	IsOnline(ctx context.Context, deviceID string) (bool, error)

	// OnlineForUser returns the online subset of a user's devices, as a set for
	// cheap membership testing when rendering a device list.
	OnlineForUser(ctx context.Context, userID string) (map[string]bool, error)
}

// PromptDispatcher is the low-latency delivery path for prompts.
//
// Strictly an accelerator. PostgreSQL is authoritative, so every method here may
// fail without data loss — the prompt is still delivered from the database on the
// next reconnect or reconciliation sweep (ADR-0006). Callers log dispatch
// failures and carry on rather than surfacing them to the user.
type PromptDispatcher interface {
	// Publish signals that a prompt is waiting for a device.
	Publish(ctx context.Context, deviceID, promptID string) error

	// Subscribe delivers dispatch signals for any device connected to THIS
	// backend instance. Cross-instance fan-out is the reason this exists: an
	// agent on instance A must be reachable from an API call served by instance B.
	Subscribe(ctx context.Context, handler func(deviceID, promptID string)) error
}

// EventPublisher fans realtime events out to dashboard clients.
//
// Also cross-instance: a dashboard connected to instance B must see events from
// an agent connected to instance A.
type EventPublisher interface {
	// PublishToUser sends an envelope to every connection owned by a user.
	PublishToUser(ctx context.Context, userID string, env protocol.Envelope) error

	// Subscribe receives envelopes published by other instances, for delivery to
	// locally-connected clients.
	Subscribe(ctx context.Context, handler func(userID string, env protocol.Envelope)) error
}

// RateLimiter enforces request quotas.
//
// Redis-backed so the limit is global across instances. A per-instance in-memory
// limiter would let N instances permit N times the configured rate, which makes
// the setting meaningless at any scale above one.
type RateLimiter interface {
	// Allow consumes one unit for key. It returns whether the request is
	// permitted, how many remain, and when the window resets — all three, because
	// the HTTP adapter must send X-RateLimit-* headers and Retry-After.
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, remaining int, resetAt time.Time, err error)
}

// DistributedLock serialises work across instances.
//
// Needed for singleton jobs — the stale-session sweep, log retention — which
// would otherwise run on every instance simultaneously.
type DistributedLock interface {
	// Acquire returns a release function, or false if the lock is held.
	// The TTL bounds how long a crashed holder can block others.
	Acquire(ctx context.Context, key string, ttl time.Duration) (release func(context.Context), acquired bool, err error)
}

// Cache stores short-lived values.
//
// Used for OAuth state, GitHub API responses, and AUTH nonces. Never for
// business data (PROJECT.md).
type Cache interface {
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error

	// SetNX sets only if absent, reporting whether it did. This is the primitive
	// behind replay protection: an AUTH nonce that fails to set has been seen
	// before, and the check-then-set must be atomic or two concurrent replays
	// both succeed.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
}

// ErrCacheMiss is returned by Cache.Get when the key is absent.
//
// A distinct sentinel because a miss is an ordinary outcome, not a failure, and
// callers must be able to tell it from a Redis outage.
var ErrCacheMiss = domain.ErrNotFound

// TokenIssuer mints signed tokens.
type TokenIssuer interface {
	// IssueAccess mints a short-lived dashboard token.
	IssueAccess(userID string) (token string, claims domain.Claims, err error)

	// IssueDevice mints a long-lived per-device agent token. Scoped to one device
	// so a leak cannot be used to impersonate the user's browser session.
	IssueDevice(userID, deviceID string) (token string, claims domain.Claims, err error)

	// IssueRefresh mints a refresh token, returning the plaintext for the cookie
	// and the hash for storage. Returning both from one call means no caller can
	// accidentally persist the plaintext.
	IssueRefresh(userID, familyID string) (plaintext, hash string, expiresAt time.Time, err error)
}

// TokenVerifier validates tokens.
type TokenVerifier interface {
	// Verify checks the signature and expiry and returns the claims. It does NOT
	// check revocation: that needs a database lookup and belongs to the caller,
	// which keeps this pure and cheap for the common path.
	Verify(token string) (domain.Claims, error)

	// HashRefresh computes the storage hash of a refresh token plaintext.
	HashRefresh(plaintext string) string
}

// GitHubUser is the identity returned by the provider.
type GitHubUser struct {
	ID        int64
	Login     string
	Name      string
	Email     string
	AvatarURL string
}

// GitHubRepo is repository metadata from the provider.
type GitHubRepo struct {
	ID            int64
	FullName      string
	DefaultBranch string
	Private       bool
	Description   string
	UpdatedAt     time.Time
}

// OAuthProvider performs the GitHub authorization code flow.
type OAuthProvider interface {
	// AuthCodeURL builds the redirect target, embedding the CSRF state.
	AuthCodeURL(state string) string

	// Exchange trades the code for an access token.
	Exchange(ctx context.Context, code string) (accessToken string, err error)

	// CurrentUser reads the authenticated identity.
	CurrentUser(ctx context.Context, accessToken string) (GitHubUser, error)

	// ListRepos reads repository metadata. Read-only: Beuvian never writes to a
	// user's GitHub account.
	ListRepos(ctx context.Context, accessToken string, limit int) ([]GitHubRepo, error)
}

// ConnectionRegistry tracks WebSocket connections on THIS instance.
//
// Deliberately instance-local. Cross-instance delivery goes through
// EventPublisher and PromptDispatcher, which keeps the hot path — writing to a
// socket this process owns — a plain in-memory map lookup with no network round
// trip.
type ConnectionRegistry interface {
	// SendToDevice writes to a device's connection on this instance, reporting
	// false if it is not connected here.
	SendToDevice(deviceID string, env protocol.Envelope) bool

	// SendToUser writes to every dashboard connection for a user on this
	// instance, returning how many received it.
	SendToUser(userID string, env protocol.Envelope) int

	// DeviceConnected reports local connectivity.
	DeviceConnected(deviceID string) bool

	// CloseDevice terminates a device's connection, used on revocation.
	CloseDevice(deviceID string, reason string)

	// Count returns the number of live connections, for health reporting.
	Count() int
}

// NotificationChannel delivers a notification through one medium.
//
// This is the extension point for the future integrations in PROJECT.md. The MVP
// has one implementation (the dashboard, over WebSocket); WhatsApp, Telegram,
// Slack, Discord, and push each become an additional implementation with no
// change to the notification use case.
type NotificationChannel interface {
	// Name identifies the channel in Notification.DeliveredChannels.
	Name() string

	// Deliver sends the notification. An error is logged and recorded, never
	// propagated to the user: a failed WhatsApp delivery must not fail the coding
	// session that triggered it.
	Deliver(ctx context.Context, n domain.Notification) error

	// Enabled reports whether the user's settings permit this channel.
	Enabled(s domain.UserSettings) bool
}
