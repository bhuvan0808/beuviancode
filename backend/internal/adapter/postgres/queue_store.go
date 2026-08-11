package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// PromptQueueStore implements port.PromptQueueStore.
//
// This is the durable system of record for prompts. Redis only accelerates
// dispatch, so every write here must succeed before the user is acknowledged —
// see docs/adr/0006-prompt-queue-postgres-authoritative.md.
type PromptQueueStore struct{ pool *pgxpool.Pool }

// NewPromptQueueStore returns a PromptQueueStore.
func NewPromptQueueStore(pool *pgxpool.Pool) *PromptQueueStore {
	return &PromptQueueStore{pool: pool}
}

var _ port.PromptQueueStore = (*PromptQueueStore)(nil)

const promptColumns = `
	id, user_id, device_id, COALESCE(session_id, ''), text, status, attempts,
	enqueued_at, dispatched_at, delivered_at, COALESCE(error, ''),
	COALESCE(correlation_id, ''), created_at, updated_at`

func scanPrompt(row interface{ Scan(...any) error }) (domain.QueuedPrompt, error) {
	var p domain.QueuedPrompt
	var status string
	err := row.Scan(
		&p.ID, &p.UserID, &p.DeviceID, &p.SessionID, &p.Text, &status, &p.Attempts,
		&p.EnqueuedAt, &p.DispatchedAt, &p.DeliveredAt, &p.Error,
		&p.CorrelationID, &p.CreatedAt, &p.UpdatedAt)
	p.Status = domain.PromptStatus(status)
	return p, err
}

// Enqueue commits a prompt durably.
//
// This must return successfully before the API responds 202. Once it has, the
// instruction cannot be lost: even if Redis then fails entirely, the prompt is
// found by PendingForDevice on the agent's next reconnect.
func (s *PromptQueueStore) Enqueue(ctx context.Context, p domain.QueuedPrompt) error {
	if err := p.Validate(); err != nil {
		return err
	}
	const q = `
		INSERT INTO prompt_queue
			(id, user_id, device_id, session_id, text, status, enqueued_at, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	_, err := s.pool.Exec(ctx, q,
		p.ID, p.UserID, p.DeviceID, nullable(p.SessionID), p.Text,
		string(p.Status), p.EnqueuedAt, nullable(p.CorrelationID))
	return translateError(err, "prompt")
}

// ByIDForUser loads a prompt scoped to its owner.
func (s *PromptQueueStore) ByIDForUser(ctx context.Context, promptID, userID string) (domain.QueuedPrompt, error) {
	q := `SELECT ` + promptColumns + ` FROM prompt_queue WHERE id = $1 AND user_id = $2`
	p, err := scanPrompt(s.pool.QueryRow(ctx, q, promptID, userID))
	return p, translateError(err, "prompt")
}

// Update persists delivery state.
func (s *PromptQueueStore) Update(ctx context.Context, p domain.QueuedPrompt) error {
	const q = `
		UPDATE prompt_queue
		SET status = $2, attempts = $3, dispatched_at = $4, delivered_at = $5, error = $6
		WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q,
		p.ID, string(p.Status), p.Attempts, p.DispatchedAt, p.DeliveredAt, nullable(p.Error))
	if err != nil {
		return translateError(err, "prompt")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// PendingForDevice returns undelivered prompts, oldest first.
//
// This is the reconciliation path that makes Redis disposable. It deliberately
// includes 'dispatched' as well as 'pending': a prompt sent to a device that then
// disconnected before acknowledging must be redelivered, and treating dispatched
// as terminal would strand it forever.
//
// Prompts that exhausted their attempts are excluded so a permanently broken
// device does not accumulate an infinite redelivery backlog.
func (s *PromptQueueStore) PendingForDevice(ctx context.Context, deviceID string, limit int) ([]domain.QueuedPrompt, error) {
	q := `SELECT ` + promptColumns + `
	      FROM prompt_queue
	      WHERE device_id = $1
	        AND status IN ('pending', 'dispatched')
	        AND attempts < $2
	      ORDER BY enqueued_at ASC
	      LIMIT $3`

	rows, err := s.pool.Query(ctx, q, deviceID, domain.MaxDeliveryAttempts, clampLimit(limit))
	if err != nil {
		return nil, translateError(err, "prompt")
	}
	defer rows.Close()

	out := make([]domain.QueuedPrompt, 0, 8)
	for rows.Next() {
		p, err := scanPrompt(rows)
		if err != nil {
			return nil, translateError(err, "prompt")
		}
		out = append(out, p)
	}
	return out, translateError(rows.Err(), "prompt")
}

// ListForUser returns a user's prompts, newest first.
func (s *PromptQueueStore) ListForUser(ctx context.Context, userID, deviceID string, p port.Page) ([]domain.QueuedPrompt, string, error) {
	limit := clampLimit(p.Limit)

	q := `SELECT ` + promptColumns + ` FROM prompt_queue WHERE user_id = $1`
	args := []any{userID}
	if deviceID != "" {
		args = append(args, deviceID)
		q += ` AND device_id = $2`
	}
	if p.Cursor != "" {
		args = append(args, p.Cursor)
		q += ` AND id < $` + itoa(len(args))
	}
	args = append(args, limit+1)
	q += ` ORDER BY id DESC LIMIT $` + itoa(len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", translateError(err, "prompt")
	}
	defer rows.Close()

	out := make([]domain.QueuedPrompt, 0, limit)
	for rows.Next() {
		item, err := scanPrompt(rows)
		if err != nil {
			return nil, "", translateError(err, "prompt")
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", translateError(err, "prompt")
	}

	var next string
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// CountPending returns the queue depth for a device, for the dashboard readout.
func (s *PromptQueueStore) CountPending(ctx context.Context, deviceID string) (int, error) {
	const q = `
		SELECT COUNT(*) FROM prompt_queue
		WHERE device_id = $1 AND status IN ('pending', 'dispatched')`
	var n int
	err := s.pool.QueryRow(ctx, q, deviceID).Scan(&n)
	return n, translateError(err, "prompt")
}

// ---------------------------------------------------------------------------

// NotificationStore implements port.NotificationStore.
type NotificationStore struct{ pool *pgxpool.Pool }

// NewNotificationStore returns a NotificationStore.
func NewNotificationStore(pool *pgxpool.Pool) *NotificationStore {
	return &NotificationStore{pool: pool}
}

var _ port.NotificationStore = (*NotificationStore)(nil)

const notificationColumns = `
	id, user_id, COALESCE(device_id, ''), COALESCE(session_id, ''), kind, title,
	COALESCE(body, ''), severity, read_at, delivered_channels, created_at, updated_at`

func scanNotification(row interface{ Scan(...any) error }) (domain.Notification, error) {
	var n domain.Notification
	var severity string
	err := row.Scan(
		&n.ID, &n.UserID, &n.DeviceID, &n.SessionID, &n.Kind, &n.Title,
		&n.Body, &severity, &n.ReadAt, &n.DeliveredChannels, &n.CreatedAt, &n.UpdatedAt)
	n.Severity = protocol.NotificationSeverity(severity)
	return n, err
}

// Create records a notification.
func (s *NotificationStore) Create(ctx context.Context, n domain.Notification) error {
	if err := n.Validate(); err != nil {
		return err
	}
	const q = `
		INSERT INTO notifications
			(id, user_id, device_id, session_id, kind, title, body, severity, delivered_channels)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	channels := n.DeliveredChannels
	if channels == nil {
		channels = []string{} // the column is NOT NULL
	}
	_, err := s.pool.Exec(ctx, q,
		n.ID, n.UserID, nullable(n.DeviceID), nullable(n.SessionID),
		n.Kind, n.Title, nullable(n.Body), string(n.Severity), channels)
	return translateError(err, "notification")
}

// ListForUser returns notifications, newest first.
func (s *NotificationStore) ListForUser(ctx context.Context, userID string, unreadOnly bool, p port.Page) ([]domain.Notification, string, error) {
	limit := clampLimit(p.Limit)

	q := `SELECT ` + notificationColumns + ` FROM notifications WHERE user_id = $1`
	args := []any{userID}
	if unreadOnly {
		q += ` AND read_at IS NULL`
	}
	if p.Cursor != "" {
		args = append(args, p.Cursor)
		q += ` AND id < $` + itoa(len(args))
	}
	args = append(args, limit+1)
	q += ` ORDER BY id DESC LIMIT $` + itoa(len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", translateError(err, "notification")
	}
	defer rows.Close()

	out := make([]domain.Notification, 0, limit)
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, "", translateError(err, "notification")
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, "", translateError(err, "notification")
	}

	var next string
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// MarkRead marks one notification read.
//
// The `read_at IS NULL` guard makes it idempotent, so a client retrying does not
// shift the timestamp and reorder the user's history.
func (s *NotificationStore) MarkRead(ctx context.Context, notificationID, userID string, at time.Time) error {
	const q = `
		UPDATE notifications SET read_at = $3
		WHERE id = $1 AND user_id = $2 AND read_at IS NULL`
	_, err := s.pool.Exec(ctx, q, notificationID, userID, at)
	return translateError(err, "notification")
}

// MarkAllRead clears a user's unread notifications.
func (s *NotificationStore) MarkAllRead(ctx context.Context, userID string, at time.Time) error {
	const q = `UPDATE notifications SET read_at = $2 WHERE user_id = $1 AND read_at IS NULL`
	_, err := s.pool.Exec(ctx, q, userID, at)
	return translateError(err, "notification")
}

// CountUnread powers the dashboard badge.
func (s *NotificationStore) CountUnread(ctx context.Context, userID string) (int, error) {
	const q = `SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`
	var n int
	err := s.pool.QueryRow(ctx, q, userID).Scan(&n)
	return n, translateError(err, "notification")
}
