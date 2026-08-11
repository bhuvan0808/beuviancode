package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
)

// DeviceStore implements port.DeviceStore.
type DeviceStore struct{ pool *pgxpool.Pool }

// NewDeviceStore returns a DeviceStore.
func NewDeviceStore(pool *pgxpool.Pool) *DeviceStore { return &DeviceStore{pool: pool} }

var _ port.DeviceStore = (*DeviceStore)(nil)

// deviceColumns is the shared projection, so every scan reads the same shape.
// Duplicating this list across queries is how a column gets added to one SELECT
// and forgotten in another, producing a scan mismatch at runtime.
const deviceColumns = `
	id, user_id, name, platform, COALESCE(agent_version, ''), capabilities,
	token_hash, token_expires_at, last_seen_at, revoked_at,
	created_at, updated_at, deleted_at`

func scanDevice(row interface{ Scan(...any) error }) (domain.Device, error) {
	var d domain.Device
	err := row.Scan(
		&d.ID, &d.UserID, &d.Name, &d.Platform, &d.AgentVersion, &d.Capabilities,
		&d.TokenHash, &d.TokenExpiresAt, &d.LastSeenAt, &d.RevokedAt,
		&d.CreatedAt, &d.UpdatedAt, &d.DeletedAt)
	return d, err
}

// Create registers a device.
func (s *DeviceStore) Create(ctx context.Context, d domain.Device) error {
	if err := d.Validate(); err != nil {
		return err
	}
	const q = `
		INSERT INTO devices
			(id, user_id, name, platform, agent_version, capabilities, token_hash, token_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	// nil marshals to SQL NULL and capabilities is NOT NULL. See the note in
	// UserStore.UpsertByGitHub: for this schema, nil and empty mean the same thing.
	_, err := s.pool.Exec(ctx, q,
		d.ID, d.UserID, d.Name, d.Platform, d.AgentVersion,
		nonNilStrings(d.Capabilities), d.TokenHash, d.TokenExpiresAt)
	return translateError(err, "device")
}

// ByID loads a device regardless of owner.
//
// Used only on the WebSocket authentication path, where the caller is
// authenticated as the device itself rather than as a user. Every user-facing
// path must use ByIDForUser instead.
func (s *DeviceStore) ByID(ctx context.Context, deviceID string) (domain.Device, error) {
	q := `SELECT ` + deviceColumns + ` FROM devices WHERE id = $1 AND deleted_at IS NULL`
	d, err := scanDevice(s.pool.QueryRow(ctx, q, deviceID))
	return d, translateError(err, "device")
}

// ByIDForUser loads a device scoped to its owner.
//
// Ownership is part of the WHERE clause rather than a check after the fetch, so no
// handler can forget it. A device belonging to someone else returns ErrNotFound,
// not ErrForbidden: a 403 would confirm the ID is real and let an attacker
// enumerate valid identifiers.
func (s *DeviceStore) ByIDForUser(ctx context.Context, deviceID, userID string) (domain.Device, error) {
	q := `SELECT ` + deviceColumns + `
	      FROM devices WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
	d, err := scanDevice(s.pool.QueryRow(ctx, q, deviceID, userID))
	return d, translateError(err, "device")
}

// ListForUser returns a user's devices, most recently seen first.
func (s *DeviceStore) ListForUser(ctx context.Context, userID string) ([]domain.Device, error) {
	q := `SELECT ` + deviceColumns + `
	      FROM devices
	      WHERE user_id = $1 AND deleted_at IS NULL
	      ORDER BY last_seen_at DESC NULLS LAST, created_at DESC`

	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, translateError(err, "device")
	}
	defer rows.Close()

	// Non-nil empty slice so the JSON encoder emits [] rather than null; a client
	// iterating the response should not have to special-case an empty list.
	out := make([]domain.Device, 0, 4)
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, translateError(err, "device")
		}
		out = append(out, d)
	}
	return out, translateError(rows.Err(), "device")
}

// Update persists mutable device fields.
//
// Deliberately excludes token_hash and token_expires_at: credential changes go
// through registration or revocation, which have their own audit trail. Allowing a
// general update to silently rotate a token would make that trail incomplete.
func (s *DeviceStore) Update(ctx context.Context, d domain.Device) error {
	if err := d.Validate(); err != nil {
		return err
	}
	const q = `
		UPDATE devices
		SET name = $3, agent_version = $4, capabilities = $5, platform = $6
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`

	tag, err := s.pool.Exec(ctx, q, d.ID, d.UserID, d.Name, d.AgentVersion,
		nonNilStrings(d.Capabilities), d.Platform)
	if err != nil {
		return translateError(err, "device")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Revoke invalidates a device's credentials without deleting it.
//
// Idempotent via `revoked_at IS NULL`, so a repeated revoke does not move the
// timestamp and lose the original revocation time.
func (s *DeviceStore) Revoke(ctx context.Context, deviceID, userID string, at time.Time) error {
	const q = `
		UPDATE devices SET revoked_at = $3
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL AND revoked_at IS NULL`

	tag, err := s.pool.Exec(ctx, q, deviceID, userID, at)
	if err != nil {
		return translateError(err, "device")
	}
	if tag.RowsAffected() == 0 {
		// Either absent, not owned, or already revoked. All three are "nothing
		// more to do" from the caller's perspective; distinguishing them would
		// leak whether the ID exists.
		return domain.ErrNotFound
	}
	return nil
}

// SoftDelete removes a device from the user's list.
//
// Also sets revoked_at: a deleted device must not keep a working credential, and
// leaving it valid would be a real security hole rather than a cosmetic one.
func (s *DeviceStore) SoftDelete(ctx context.Context, deviceID, userID string, at time.Time) error {
	const q = `
		UPDATE devices
		SET deleted_at = $3, revoked_at = COALESCE(revoked_at, $3)
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`

	tag, err := s.pool.Exec(ctx, q, deviceID, userID, at)
	if err != nil {
		return translateError(err, "device")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// TouchLastSeen records liveness.
//
// Called on every heartbeat, so it is deliberately the cheapest possible write:
// one indexed update, no read, no transaction. Presence itself lives in Redis;
// this column is the durable fallback and the "last seen" display value.
func (s *DeviceStore) TouchLastSeen(ctx context.Context, deviceID string, at time.Time) error {
	const q = `UPDATE devices SET last_seen_at = $2 WHERE id = $1 AND deleted_at IS NULL`
	_, err := s.pool.Exec(ctx, q, deviceID, at)
	return translateError(err, "device")
}

// SaveStatus upserts the device's latest resource snapshot.
//
// One row per device, updated in place. The `reported_at <=` guard discards frames
// that arrive out of order — under reconnect and redelivery an older STATUS can
// land after a newer one, and without this the dashboard would flicker backwards.
func (s *DeviceStore) SaveStatus(ctx context.Context, st domain.AgentStatus) error {
	const q = `
		INSERT INTO agent_status
			(device_id, state, adapter, repository, current_task,
			 cpu_percent, memory_bytes, queued_prompts, session_id, reported_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (device_id) DO UPDATE SET
			state          = EXCLUDED.state,
			adapter        = EXCLUDED.adapter,
			repository     = EXCLUDED.repository,
			current_task   = EXCLUDED.current_task,
			cpu_percent    = EXCLUDED.cpu_percent,
			memory_bytes   = EXCLUDED.memory_bytes,
			queued_prompts = EXCLUDED.queued_prompts,
			session_id     = EXCLUDED.session_id,
			reported_at    = EXCLUDED.reported_at
		WHERE agent_status.reported_at <= EXCLUDED.reported_at`

	_, err := s.pool.Exec(ctx, q,
		st.DeviceID, st.State, nullable(st.Adapter), nullable(st.Repository),
		nullable(st.CurrentTask), st.CPUPercent, int64(st.MemoryBytes),
		st.QueuedPrompts, nullable(st.SessionID), st.ReportedAt)
	return translateError(err, "agent_status")
}

// Status loads a device's latest snapshot.
func (s *DeviceStore) Status(ctx context.Context, deviceID string) (domain.AgentStatus, error) {
	const q = `
		SELECT device_id, state, COALESCE(adapter, ''), COALESCE(repository, ''),
		       COALESCE(current_task, ''), cpu_percent, memory_bytes, queued_prompts,
		       COALESCE(session_id, ''), reported_at, updated_at
		FROM agent_status WHERE device_id = $1`

	var st domain.AgentStatus
	var mem int64
	err := s.pool.QueryRow(ctx, q, deviceID).Scan(
		&st.DeviceID, &st.State, &st.Adapter, &st.Repository, &st.CurrentTask,
		&st.CPUPercent, &mem, &st.QueuedPrompts, &st.SessionID, &st.ReportedAt, &st.UpdatedAt)
	if err != nil {
		return domain.AgentStatus{}, translateError(err, "agent_status")
	}
	st.MemoryBytes = uint64(mem)
	return st, nil
}

// StatusForUser loads snapshots for all of a user's devices in one query.
//
// A single join rather than N lookups: rendering the device list is the
// dashboard's most frequent call, and per-device queries would make it scale with
// the number of machines a user owns.
func (s *DeviceStore) StatusForUser(ctx context.Context, userID string) (map[string]domain.AgentStatus, error) {
	const q = `
		SELECT a.device_id, a.state, COALESCE(a.adapter, ''), COALESCE(a.repository, ''),
		       COALESCE(a.current_task, ''), a.cpu_percent, a.memory_bytes, a.queued_prompts,
		       COALESCE(a.session_id, ''), a.reported_at, a.updated_at
		FROM agent_status a
		JOIN devices d ON d.id = a.device_id
		WHERE d.user_id = $1 AND d.deleted_at IS NULL`

	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, translateError(err, "agent_status")
	}
	defer rows.Close()

	out := make(map[string]domain.AgentStatus)
	for rows.Next() {
		var st domain.AgentStatus
		var mem int64
		if err := rows.Scan(
			&st.DeviceID, &st.State, &st.Adapter, &st.Repository, &st.CurrentTask,
			&st.CPUPercent, &mem, &st.QueuedPrompts, &st.SessionID,
			&st.ReportedAt, &st.UpdatedAt); err != nil {
			return nil, translateError(err, "agent_status")
		}
		st.MemoryBytes = uint64(mem)
		out[st.DeviceID] = st
	}
	return out, translateError(rows.Err(), "agent_status")
}
