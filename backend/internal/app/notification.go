package app

import (
	"context"
	"log/slog"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// NotificationService creates notifications and fans them out over channels.
//
// The channel list is the extension point PROJECT.md requires: the MVP registers
// only the dashboard channel, and WhatsApp, Telegram, Slack, Discord, and push each
// become an additional port.NotificationChannel with no change to this service.
type NotificationService struct {
	notifications port.NotificationStore
	users         port.UserStore
	channels      []port.NotificationChannel
	ids           port.IDGenerator
	clock         port.Clock
	log           *slog.Logger
}

// NotificationDeps groups NotificationService's collaborators.
type NotificationDeps struct {
	Notifications port.NotificationStore
	Users         port.UserStore
	Channels      []port.NotificationChannel
	IDs           port.IDGenerator
	Clock         port.Clock
	Log           *slog.Logger
}

// NewNotificationService builds a NotificationService.
func NewNotificationService(d NotificationDeps) *NotificationService {
	return &NotificationService{
		notifications: d.Notifications, users: d.Users, channels: d.Channels,
		ids: d.IDs, clock: d.Clock,
		log: d.Log.With(slog.String("service", "notification")),
	}
}

// NotifyInput describes a notification to raise.
type NotifyInput struct {
	UserID    string
	DeviceID  string
	SessionID string
	Kind      string
	Title     string
	Body      string
	Severity  protocol.NotificationSeverity
}

// Notify creates a notification and delivers it over every enabled channel.
//
// User preferences are consulted BEFORE the row is created, not just before
// delivery. Storing notifications a user has disabled would make the unread badge
// wrong and the notifications page noisy with things they explicitly opted out of.
func (s *NotificationService) Notify(ctx context.Context, in NotifyInput) (domain.Notification, error) {
	now := s.clock.Now()

	n := domain.Notification{
		ID:                s.ids.NewID(id.PrefixNotification),
		UserID:            in.UserID,
		DeviceID:          in.DeviceID,
		SessionID:         in.SessionID,
		Kind:              in.Kind,
		Title:             in.Title,
		Body:              in.Body,
		Severity:          in.Severity,
		DeliveredChannels: []string{},
		CreatedAt:         now,
	}
	if err := n.Validate(); err != nil {
		return domain.Notification{}, err
	}

	settings, err := s.users.Settings(ctx, in.UserID)
	if err != nil {
		// Settings failing should not swallow a notification. Defaults have both
		// completion and waiting alerts enabled, which is the safe direction: a
		// missed "your agent is waiting for you" defeats the product.
		s.log.Warn("settings lookup failed; using defaults", blog.Err(err))
		settings = domain.DefaultSettings(in.UserID)
	}
	if !n.WantedBy(settings) {
		s.log.Debug("notification suppressed by user settings",
			slog.String("kind", n.Kind), slog.String("user_id", in.UserID))
		return n, nil
	}

	for _, ch := range s.channels {
		if !ch.Enabled(settings) {
			continue
		}
		if err := ch.Deliver(ctx, n); err != nil {
			// Never propagated. A failed WhatsApp delivery must not fail the coding
			// session that triggered it.
			s.log.Warn("notification channel failed",
				slog.String("channel", ch.Name()), blog.Err(err))
			continue
		}
		n.DeliveredChannels = append(n.DeliveredChannels, ch.Name())
	}

	if err := s.notifications.Create(ctx, n); err != nil {
		return domain.Notification{}, err
	}
	return n, nil
}

// List returns a user's notifications.
func (s *NotificationService) List(ctx context.Context, userID string, unreadOnly bool, p port.Page) ([]domain.Notification, string, error) {
	return s.notifications.ListForUser(ctx, userID, unreadOnly, p)
}

// MarkRead marks one notification read.
func (s *NotificationService) MarkRead(ctx context.Context, notificationID, userID string) error {
	return s.notifications.MarkRead(ctx, notificationID, userID, s.clock.Now())
}

// MarkAllRead clears a user's unread notifications.
func (s *NotificationService) MarkAllRead(ctx context.Context, userID string) error {
	return s.notifications.MarkAllRead(ctx, userID, s.clock.Now())
}

// CountUnread powers the dashboard badge.
func (s *NotificationService) CountUnread(ctx context.Context, userID string) (int, error) {
	return s.notifications.CountUnread(ctx, userID)
}

// ---------------------------------------------------------------------------

// DashboardChannel delivers notifications over the realtime connection.
//
// The MVP's only channel. It writes to whichever backend instance holds the user's
// dashboard socket, going through EventPublisher rather than the local registry so
// a user connected to another instance still receives it.
type DashboardChannel struct {
	events port.EventPublisher
	ids    port.IDGenerator
	clock  port.Clock
}

// NewDashboardChannel builds the dashboard notification channel.
func NewDashboardChannel(events port.EventPublisher, ids port.IDGenerator, clock port.Clock) *DashboardChannel {
	return &DashboardChannel{events: events, ids: ids, clock: clock}
}

var _ port.NotificationChannel = (*DashboardChannel)(nil)

// Name identifies the channel in DeliveredChannels.
func (c *DashboardChannel) Name() string { return "dashboard" }

// Enabled reports whether the channel applies.
//
// Always true: the dashboard is the baseline channel, and a user who disabled it
// would have no way to see notifications at all.
func (c *DashboardChannel) Enabled(domain.UserSettings) bool { return true }

// Deliver publishes the notification to the user's dashboard connections.
func (c *DashboardChannel) Deliver(ctx context.Context, n domain.Notification) error {
	env, err := protocol.NewEnvelope(
		c.ids.NewID(id.PrefixMessage), protocol.TypeNotification, c.clock.Now(),
		protocol.NotificationPayload{
			NotificationID: n.ID,
			Kind:           n.Kind,
			Title:          n.Title,
			Body:           n.Body,
			Severity:       n.Severity,
			CreatedAt:      n.CreatedAt,
		})
	if err != nil {
		return err
	}
	env.DeviceID = n.DeviceID
	env.SessionID = n.SessionID
	env.CorrelationID = blog.CorrelationIDFrom(ctx)

	return c.events.PublishToUser(ctx, n.UserID, env)
}
