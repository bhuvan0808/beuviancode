package port

import (
	"context"
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
)

// Clock abstracts time.
//
// Injected rather than calling time.Now() directly so token expiry, session
// staleness, and rate-limit windows are testable without sleeping. A test that
// sleeps for a real timeout is slow and flaky; one that advances a fake clock is
// neither.
type Clock interface {
	Now() time.Time
}

// IDGenerator abstracts identifier minting, so tests can produce deterministic
// IDs and assertions can name them.
type IDGenerator interface {
	NewID(prefix string) string
}

// Page describes a cursor-paginated request.
//
// Cursor rather than offset: offsets skip or duplicate rows when data is inserted
// between pages, which for append-only session and log data is guaranteed rather
// than unlikely.
type Page struct {
	Limit  int
	Cursor string
}

// UserStore persists users and their linked identities.
type UserStore interface {
	// UpsertByGitHub creates or updates a user from a GitHub identity, keyed on
	// the numeric GitHub ID. One method rather than separate find/create because
	// login is a single logical operation and splitting it invites a race where
	// two concurrent logins both create a user.
	UpsertByGitHub(ctx context.Context, u domain.User, acct domain.OAuthAccount) (domain.User, error)

	ByID(ctx context.Context, id string) (domain.User, error)
	Settings(ctx context.Context, userID string) (domain.UserSettings, error)
	SaveSettings(ctx context.Context, s domain.UserSettings) error
	OAuthAccount(ctx context.Context, userID string, p domain.OAuthProvider) (domain.OAuthAccount, error)
}

// DeviceStore persists devices and their reported status.
type DeviceStore interface {
	Create(ctx context.Context, d domain.Device) error
	ByID(ctx context.Context, id string) (domain.Device, error)

	// ByIDForUser scopes the lookup to an owner, returning ErrNotFound when the
	// device exists but belongs to someone else. Enforcing ownership in the query
	// rather than after it means no handler can forget the check.
	ByIDForUser(ctx context.Context, id, userID string) (domain.Device, error)

	ListForUser(ctx context.Context, userID string) ([]domain.Device, error)
	Update(ctx context.Context, d domain.Device) error
	Revoke(ctx context.Context, id, userID string, at time.Time) error
	SoftDelete(ctx context.Context, id, userID string, at time.Time) error
	TouchLastSeen(ctx context.Context, id string, at time.Time) error

	SaveStatus(ctx context.Context, s domain.AgentStatus) error
	Status(ctx context.Context, deviceID string) (domain.AgentStatus, error)
	StatusForUser(ctx context.Context, userID string) (map[string]domain.AgentStatus, error)
}

// RepositoryStore persists repositories.
type RepositoryStore interface {
	Create(ctx context.Context, r domain.Repository) error
	ByIDForUser(ctx context.Context, id, userID string) (domain.Repository, error)
	ListForUser(ctx context.Context, userID string) ([]domain.Repository, error)
	Update(ctx context.Context, r domain.Repository) error
	SoftDelete(ctx context.Context, id, userID string, at time.Time) error
}

// SessionFilter narrows a session listing.
type SessionFilter struct {
	UserID     string
	DeviceID   string
	ActiveOnly bool
}

// SessionStore persists sessions.
type SessionStore interface {
	Create(ctx context.Context, s domain.Session) error

	// ByID loads a session without an ownership check.
	//
	// For agent-driven paths only, where the caller is authenticated as the device
	// and has already been matched to the session's device. Every user-facing path
	// must use ByIDForUser instead, which scopes the lookup in the query.
	ByID(ctx context.Context, id string) (domain.Session, error)

	ByIDForUser(ctx context.Context, id, userID string) (domain.Session, error)
	Update(ctx context.Context, s domain.Session) error
	List(ctx context.Context, f SessionFilter, p Page) ([]domain.Session, string, error)

	// ActiveForDevice returns the running session, or ErrNotFound. Used to reject
	// starting a second session on a device that already has one.
	ActiveForDevice(ctx context.Context, deviceID string) (domain.Session, error)

	// EndStaleSessions closes sessions whose device stopped reporting. Without
	// this, a crashed agent leaves a session that appears to be running forever.
	EndStaleSessions(ctx context.Context, staleBefore time.Time) (int, error)
}

// SessionLogStore persists session output.
type SessionLogStore interface {
	// AppendBatch inserts log rows idempotently: a batch redelivered after a
	// reconnect conflicts on (session_id, seq) and is skipped rather than
	// duplicating the transcript.
	AppendBatch(ctx context.Context, logs []domain.SessionLog) error

	// After returns log rows with seq greater than afterSeq, for history and for
	// a dashboard joining mid-session. Paged by seq, not timestamp: timestamps
	// collide under load and are not monotonic across a clock adjustment.
	After(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]domain.SessionLog, error)

	// MaxSeq returns the highest seq stored, so ingestion can continue the
	// sequence after a backend restart.
	MaxSeq(ctx context.Context, sessionID string) (int64, error)

	DeleteOlderThan(ctx context.Context, before time.Time) (int64, error)
}

// MessageStore persists the human-readable conversation.
type MessageStore interface {
	Create(ctx context.Context, m domain.Message) error
	ListForSession(ctx context.Context, sessionID string, p Page) ([]domain.Message, string, error)
}

// PromptQueueStore is the durable system of record for prompts (ADR-0006).
type PromptQueueStore interface {
	// Enqueue commits the prompt. This must complete before the API acknowledges
	// the user, so a subsequent Redis failure cannot lose the instruction.
	Enqueue(ctx context.Context, p domain.QueuedPrompt) error

	ByIDForUser(ctx context.Context, id, userID string) (domain.QueuedPrompt, error)
	Update(ctx context.Context, p domain.QueuedPrompt) error

	// PendingForDevice returns undelivered prompts oldest-first. This is the
	// reconciliation path that makes Redis disposable: anything Redis lost is
	// still found here.
	PendingForDevice(ctx context.Context, deviceID string, limit int) ([]domain.QueuedPrompt, error)

	ListForUser(ctx context.Context, userID, deviceID string, p Page) ([]domain.QueuedPrompt, string, error)
	CountPending(ctx context.Context, deviceID string) (int, error)
}

// NotificationStore persists notifications.
type NotificationStore interface {
	Create(ctx context.Context, n domain.Notification) error
	ListForUser(ctx context.Context, userID string, unreadOnly bool, p Page) ([]domain.Notification, string, error)
	MarkRead(ctx context.Context, id, userID string, at time.Time) error
	MarkAllRead(ctx context.Context, userID string, at time.Time) error
	CountUnread(ctx context.Context, userID string) (int, error)
}

// RefreshTokenStore persists refresh tokens by hash.
type RefreshTokenStore interface {
	Create(ctx context.Context, t domain.RefreshToken) error

	// ByHash looks a token up by its SHA-256. The plaintext is never stored, so
	// this is the only way to find one.
	ByHash(ctx context.Context, hash string) (domain.RefreshToken, error)

	MarkUsed(ctx context.Context, id string, at time.Time) error

	// RevokeFamily deletes every token in a rotation lineage. Called on reuse
	// detection, which is treated as theft rather than as a client mistake.
	RevokeFamily(ctx context.Context, familyID string) error

	RevokeAllForUser(ctx context.Context, userID string) error
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

// AuditLogger records security-relevant actions.
//
// Writes must never fail a request: an audit write that returns an error to the
// user would make auditing a liability. Implementations log and swallow.
type AuditLogger interface {
	Record(ctx context.Context, e domain.AuditEntry)
}
