package domain

import (
	"strings"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// Notification kinds.
//
// Machine-readable strings rather than display labels, because the planned
// WhatsApp, Telegram, Slack, and push channels must route on these without
// parsing prose.
const (
	KindTaskComplete  = "task_complete"
	KindTaskWaiting   = "task_waiting"
	KindDeviceOffline = "device_offline"
	KindDeviceOnline  = "device_online"
	KindSessionFailed = "session_failed"
	KindPromptFailed  = "prompt_failed"

	// KindSessionStopRequested is a control instruction sent TO an agent rather
	// than a user-facing notification.
	//
	// The protocol fixes the message set at 13 types (PROJECT.md), and widening it
	// for one control action is not worth breaking that contract. Reusing
	// NOTIFICATION works precisely because `kind` is a stable machine-readable
	// identifier rather than a display label, so the agent can route on it.
	KindSessionStopRequested = "session_stop_requested"
)

// Notification is a user-facing event.
type Notification struct {
	ID        string
	UserID    string
	DeviceID  string
	SessionID string

	Kind     string
	Title    string
	Body     string
	Severity protocol.NotificationSeverity

	ReadAt *time.Time

	// DeliveredChannels records which channels actually delivered this. Present
	// now so adding a channel later needs no migration.
	DeliveredChannels []string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate checks notification invariants.
func (n *Notification) Validate() error {
	if n.UserID == "" {
		return Invalid("user_id", "must be set")
	}
	if strings.TrimSpace(n.Kind) == "" {
		return Invalid("kind", "must not be empty")
	}
	if strings.TrimSpace(n.Title) == "" {
		return Invalid("title", "must not be empty")
	}
	switch n.Severity {
	case protocol.SeverityInfo, protocol.SeverityWarning, protocol.SeverityError:
	default:
		return Invalid("severity", "must be info, warning or error")
	}
	return nil
}

// Read reports whether the user has seen it.
func (n *Notification) Read() bool { return n.ReadAt != nil }

// MarkRead is idempotent, so a client retrying does not shift the timestamp.
func (n *Notification) MarkRead(now time.Time) {
	if n.ReadAt == nil {
		n.ReadAt = &now
		n.UpdatedAt = now
	}
}

// WantedBy reports whether the user's settings permit this notification.
//
// Consulted before creating the row, not just before delivering it: storing
// notifications the user disabled would make the unread count wrong and the
// notifications page noisy.
func (n *Notification) WantedBy(s UserSettings) bool {
	switch n.Kind {
	case KindTaskComplete:
		return s.NotifyOnComplete
	case KindTaskWaiting:
		return s.NotifyOnWaiting
	case KindDeviceOffline, KindDeviceOnline:
		return s.NotifyOnDeviceOffline
	default:
		// Unknown kinds default to delivered. A new notification type should be
		// visible until someone deliberately adds a preference for it; the
		// opposite default would make new notifications silently vanish.
		return true
	}
}

// AuditEntry records a security-relevant action.
type AuditEntry struct {
	ID     int64
	UserID string

	// Action is a stable dotted identifier: "device.revoked", "prompt.sent".
	Action     string
	TargetType string
	TargetID   string

	Metadata map[string]any

	IPAddress string
	UserAgent string

	CorrelationID string
	CreatedAt     time.Time
}

// Audit action constants.
const (
	ActionUserLogin       = "user.login"
	ActionUserLogout      = "user.logout"
	ActionTokenRefreshed  = "token.refreshed"
	ActionTokenReuse      = "token.reuse_detected"
	ActionDeviceRegister  = "device.registered"
	ActionDeviceRevoked   = "device.revoked"
	ActionDeviceDeleted   = "device.deleted"
	ActionSessionStarted  = "session.started"
	ActionSessionStopped  = "session.stopped"
	ActionPromptSent      = "prompt.sent"
	ActionPromptCancelled = "prompt.cancelled"
	ActionSettingsChanged = "settings.changed"
)
