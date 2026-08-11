package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// SessionStore implements port.SessionStore.
type SessionStore struct{ pool *pgxpool.Pool }

// NewSessionStore returns a SessionStore.
func NewSessionStore(pool *pgxpool.Pool) *SessionStore { return &SessionStore{pool: pool} }

var _ port.SessionStore = (*SessionStore)(nil)

const sessionColumns = `
	id, user_id, device_id, COALESCE(repository_id, ''), adapter, state,
	COALESCE(current_task, ''), working_directory, COALESCE(pid, 0), exit_code,
	started_at, ended_at, created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (domain.Session, error) {
	var s domain.Session
	var state string
	err := row.Scan(
		&s.ID, &s.UserID, &s.DeviceID, &s.RepositoryID, &s.Adapter, &state,
		&s.CurrentTask, &s.WorkingDirectory, &s.PID, &s.ExitCode,
		&s.StartedAt, &s.EndedAt, &s.CreatedAt, &s.UpdatedAt)
	s.State = protocol.AgentState(state)
	return s, err
}

// Create starts a session.
//
// The unique partial index idx_sessions_one_active_per_device enforces at most one
// live session per device. Relying on the database rather than a read-then-write in
// application code matters: two concurrent start requests would both pass an
// application-level check and both insert.
func (s *SessionStore) Create(ctx context.Context, sess domain.Session) error {
	if err := sess.Validate(); err != nil {
		return err
	}
	const q = `
		INSERT INTO sessions
			(id, user_id, device_id, repository_id, adapter, state,
			 current_task, working_directory, pid, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	_, err := s.pool.Exec(ctx, q,
		sess.ID, sess.UserID, sess.DeviceID, nullable(sess.RepositoryID),
		sess.Adapter, string(sess.State), nullable(sess.CurrentTask),
		sess.WorkingDirectory, nullZero(sess.PID), sess.StartedAt)
	return translateError(err, "session")
}

// ByID loads a session without an ownership check.
//
// Restricted to agent-driven paths, where the caller is authenticated as the
// device and has already been matched to this session's device.
func (s *SessionStore) ByID(ctx context.Context, sessionID string) (domain.Session, error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE id = $1`
	sess, err := scanSession(s.pool.QueryRow(ctx, q, sessionID))
	return sess, translateError(err, "session")
}

// ByIDForUser loads a session scoped to its owner.
func (s *SessionStore) ByIDForUser(ctx context.Context, sessionID, userID string) (domain.Session, error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE id = $1 AND user_id = $2`
	sess, err := scanSession(s.pool.QueryRow(ctx, q, sessionID, userID))
	return sess, translateError(err, "session")
}

// Update persists session state.
func (s *SessionStore) Update(ctx context.Context, sess domain.Session) error {
	const q = `
		UPDATE sessions
		SET state = $2, current_task = $3, pid = $4, exit_code = $5,
		    ended_at = $6, repository_id = $7
		WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q,
		sess.ID, string(sess.State), nullable(sess.CurrentTask), nullZero(sess.PID),
		sess.ExitCode, sess.EndedAt, nullable(sess.RepositoryID))
	if err != nil {
		return translateError(err, "session")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// List returns sessions matching a filter, newest first, cursor-paginated.
//
// The cursor is the last ID seen. Because IDs are time-sortable ULIDs, `id < cursor`
// with `ORDER BY id DESC` is a stable page boundary — an offset would skip or repeat
// rows as new sessions are inserted between requests.
func (s *SessionStore) List(ctx context.Context, f port.SessionFilter, p port.Page) ([]domain.Session, string, error) {
	limit := clampLimit(p.Limit)

	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE user_id = $1`
	args := []any{f.UserID}

	if f.DeviceID != "" {
		args = append(args, f.DeviceID)
		q += ` AND device_id = $2`
	}
	if f.ActiveOnly {
		q += ` AND ended_at IS NULL`
	}
	if p.Cursor != "" {
		args = append(args, p.Cursor)
		q += ` AND id < $` + itoa(len(args))
	}
	// limit+1 so a full page tells us whether another exists, without a COUNT.
	args = append(args, limit+1)
	q += ` ORDER BY id DESC LIMIT $` + itoa(len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", translateError(err, "session")
	}
	defer rows.Close()

	out := make([]domain.Session, 0, limit)
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, "", translateError(err, "session")
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, "", translateError(err, "session")
	}

	var next string
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// ActiveForDevice returns the running session on a device.
func (s *SessionStore) ActiveForDevice(ctx context.Context, deviceID string) (domain.Session, error) {
	q := `SELECT ` + sessionColumns + `
	      FROM sessions WHERE device_id = $1 AND ended_at IS NULL
	      ORDER BY started_at DESC LIMIT 1`
	sess, err := scanSession(s.pool.QueryRow(ctx, q, deviceID))
	return sess, translateError(err, "session")
}

// EndStaleSessions closes sessions whose device stopped reporting.
//
// Without this, a crashed agent leaves a session that looks like it is running
// forever, and the unique partial index then blocks the user from ever starting a
// new one on that device. That makes this sweep a correctness requirement, not
// merely housekeeping.
func (s *SessionStore) EndStaleSessions(ctx context.Context, staleBefore time.Time) (int, error) {
	const q = `
		UPDATE sessions s
		SET state = 'crashed', ended_at = now()
		FROM devices d
		WHERE s.device_id = d.id
		  AND s.ended_at IS NULL
		  AND (d.last_seen_at IS NULL OR d.last_seen_at < $1)`

	tag, err := s.pool.Exec(ctx, q, staleBefore)
	if err != nil {
		return 0, translateError(err, "session")
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------

// SessionLogStore implements port.SessionLogStore.
type SessionLogStore struct{ pool *pgxpool.Pool }

// NewSessionLogStore returns a SessionLogStore.
func NewSessionLogStore(pool *pgxpool.Pool) *SessionLogStore { return &SessionLogStore{pool: pool} }

var _ port.SessionLogStore = (*SessionLogStore)(nil)

// AppendBatch inserts log rows, skipping duplicates.
//
// ON CONFLICT DO NOTHING against the (session_id, seq) unique constraint makes
// ingestion idempotent: a batch redelivered after a reconnect is discarded rather
// than duplicating the transcript. That is what allows the agent to re-send
// unacknowledged batches without coordination.
//
// Uses CopyFrom-style batching via a single multi-row INSERT because a verbose
// build emits thousands of lines per second and one round trip per line would
// saturate the connection pool.
func (s *SessionLogStore) AppendBatch(ctx context.Context, logs []domain.SessionLog) error {
	if len(logs) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	const q = `
		INSERT INTO session_logs (session_id, seq, stream, content, truncated, at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (session_id, seq) DO NOTHING`

	for _, l := range logs {
		batch.Queue(q, l.SessionID, l.Seq, string(l.Stream), l.Content, l.Truncated, l.At)
	}

	br := s.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()

	for range logs {
		if _, err := br.Exec(); err != nil {
			return translateError(err, "session_log")
		}
	}
	return nil
}

// After returns log rows following afterSeq.
//
// Paged by seq rather than timestamp: timestamps collide under load and are not
// monotonic across a clock adjustment, so paging by them can silently skip lines.
func (s *SessionLogStore) After(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]domain.SessionLog, error) {
	const q = `
		SELECT id, session_id, seq, stream, content, truncated, at, created_at
		FROM session_logs
		WHERE session_id = $1 AND seq > $2
		ORDER BY seq ASC LIMIT $3`

	rows, err := s.pool.Query(ctx, q, sessionID, afterSeq, clampLimit(limit))
	if err != nil {
		return nil, translateError(err, "session_log")
	}
	defer rows.Close()

	out := make([]domain.SessionLog, 0, 64)
	for rows.Next() {
		var l domain.SessionLog
		var stream string
		if err := rows.Scan(&l.ID, &l.SessionID, &l.Seq, &stream,
			&l.Content, &l.Truncated, &l.At, &l.CreatedAt); err != nil {
			return nil, translateError(err, "session_log")
		}
		l.Stream = protocol.LogStream(stream)
		out = append(out, l)
	}
	return out, translateError(rows.Err(), "session_log")
}

// MaxSeq returns the highest sequence stored for a session.
//
// Lets ingestion continue the sequence after a backend restart instead of
// resetting to zero, which would collide with existing rows and silently drop the
// new lines via ON CONFLICT.
func (s *SessionLogStore) MaxSeq(ctx context.Context, sessionID string) (int64, error) {
	const q = `SELECT COALESCE(MAX(seq), 0) FROM session_logs WHERE session_id = $1`
	var seq int64
	err := s.pool.QueryRow(ctx, q, sessionID).Scan(&seq)
	return seq, translateError(err, "session_log")
}

// DeleteOlderThan enforces log retention.
func (s *SessionLogStore) DeleteOlderThan(ctx context.Context, before time.Time) (int64, error) {
	const q = `DELETE FROM session_logs WHERE created_at < $1`
	tag, err := s.pool.Exec(ctx, q, before)
	if err != nil {
		return 0, translateError(err, "session_log")
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------

// MessageStore implements port.MessageStore.
type MessageStore struct{ pool *pgxpool.Pool }

// NewMessageStore returns a MessageStore.
func NewMessageStore(pool *pgxpool.Pool) *MessageStore { return &MessageStore{pool: pool} }

var _ port.MessageStore = (*MessageStore)(nil)

// Create records a conversation message.
func (s *MessageStore) Create(ctx context.Context, m domain.Message) error {
	const q = `
		INSERT INTO messages (id, session_id, user_id, role, content, prompt_id)
		VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := s.pool.Exec(ctx, q,
		m.ID, m.SessionID, nullable(m.UserID), string(m.Role), m.Content, nullable(m.PromptID))
	return translateError(err, "message")
}

// ListForSession returns messages oldest-first.
//
// Ascending, unlike sessions and notifications: a conversation reads forwards, and
// reversing it client-side for every render would be wasted work.
func (s *MessageStore) ListForSession(ctx context.Context, sessionID string, p port.Page) ([]domain.Message, string, error) {
	limit := clampLimit(p.Limit)

	q := `
		SELECT id, session_id, COALESCE(user_id, ''), role, content,
		       COALESCE(prompt_id, ''), created_at
		FROM messages WHERE session_id = $1`
	args := []any{sessionID}
	if p.Cursor != "" {
		args = append(args, p.Cursor)
		q += ` AND id > $2`
	}
	args = append(args, limit+1)
	q += ` ORDER BY id ASC LIMIT $` + itoa(len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", translateError(err, "message")
	}
	defer rows.Close()

	out := make([]domain.Message, 0, limit)
	for rows.Next() {
		var m domain.Message
		var role string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.UserID, &role,
			&m.Content, &m.PromptID, &m.CreatedAt); err != nil {
			return nil, "", translateError(err, "message")
		}
		m.Role = domain.MessageRole(role)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", translateError(err, "message")
	}

	var next string
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}
