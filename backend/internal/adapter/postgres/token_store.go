package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
)

// RefreshTokenStore implements port.RefreshTokenStore.
type RefreshTokenStore struct{ pool *pgxpool.Pool }

// NewRefreshTokenStore returns a RefreshTokenStore.
func NewRefreshTokenStore(pool *pgxpool.Pool) *RefreshTokenStore {
	return &RefreshTokenStore{pool: pool}
}

var _ port.RefreshTokenStore = (*RefreshTokenStore)(nil)

// Create stores a refresh token by hash.
func (s *RefreshTokenStore) Create(ctx context.Context, t domain.RefreshToken) error {
	const q = `
		INSERT INTO refresh_tokens
			(id, user_id, token_hash, family_id, expires_at, user_agent, ip_address)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	_, err := s.pool.Exec(ctx, q,
		t.ID, t.UserID, t.TokenHash, t.FamilyID, t.ExpiresAt,
		nullable(t.UserAgent), nullable(t.IPAddress))
	return translateError(err, "refresh_token")
}

// ByHash looks up a token by its SHA-256.
//
// The only lookup available, because the plaintext is never stored. That is what
// makes a database dump unusable as a set of logins.
func (s *RefreshTokenStore) ByHash(ctx context.Context, hash string) (domain.RefreshToken, error) {
	const q = `
		SELECT id, user_id, token_hash, family_id, expires_at, used_at,
		       COALESCE(user_agent, ''), COALESCE(ip_address, ''), created_at
		FROM refresh_tokens WHERE token_hash = $1`

	var t domain.RefreshToken
	err := s.pool.QueryRow(ctx, q, hash).Scan(
		&t.ID, &t.UserID, &t.TokenHash, &t.FamilyID, &t.ExpiresAt, &t.UsedAt,
		&t.UserAgent, &t.IPAddress, &t.CreatedAt)
	return t, translateError(err, "refresh_token")
}

// MarkUsed records that a token was rotated.
//
// The `used_at IS NULL` guard is the reuse detector's foundation: if this affects
// zero rows the token was already used, which means two parties hold it. The
// caller treats that as theft and revokes the family.
func (s *RefreshTokenStore) MarkUsed(ctx context.Context, tokenID string, at time.Time) error {
	const q = `UPDATE refresh_tokens SET used_at = $2 WHERE id = $1 AND used_at IS NULL`
	tag, err := s.pool.Exec(ctx, q, tokenID, at)
	if err != nil {
		return translateError(err, "refresh_token")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrTokenReused
	}
	return nil
}

// RevokeFamily deletes an entire rotation lineage.
//
// Deleted outright rather than flagged: keeping a revoked credential row "just in
// case" is the opposite of what revocation means, and the audit_log already records
// that the revocation happened.
func (s *RefreshTokenStore) RevokeFamily(ctx context.Context, familyID string) error {
	const q = `DELETE FROM refresh_tokens WHERE family_id = $1`
	_, err := s.pool.Exec(ctx, q, familyID)
	return translateError(err, "refresh_token")
}

// RevokeAllForUser ends every session for a user, used on logout-everywhere.
func (s *RefreshTokenStore) RevokeAllForUser(ctx context.Context, userID string) error {
	const q = `DELETE FROM refresh_tokens WHERE user_id = $1`
	_, err := s.pool.Exec(ctx, q, userID)
	return translateError(err, "refresh_token")
}

// DeleteExpired prunes tokens past their expiry.
func (s *RefreshTokenStore) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	const q = `DELETE FROM refresh_tokens WHERE expires_at < $1`
	tag, err := s.pool.Exec(ctx, q, before)
	if err != nil {
		return 0, translateError(err, "refresh_token")
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------

// RepositoryStore implements port.RepositoryStore.
type RepositoryStore struct{ pool *pgxpool.Pool }

// NewRepositoryStore returns a RepositoryStore.
func NewRepositoryStore(pool *pgxpool.Pool) *RepositoryStore {
	return &RepositoryStore{pool: pool}
}

var _ port.RepositoryStore = (*RepositoryStore)(nil)

const repoColumns = `
	id, user_id, full_name, COALESCE(local_path, ''), COALESCE(device_id, ''),
	COALESCE(default_branch, ''), COALESCE(github_id, 0), is_private,
	created_at, updated_at, deleted_at`

func scanRepo(row interface{ Scan(...any) error }) (domain.Repository, error) {
	var r domain.Repository
	err := row.Scan(
		&r.ID, &r.UserID, &r.FullName, &r.LocalPath, &r.DeviceID,
		&r.DefaultBranch, &r.GitHubID, &r.IsPrivate,
		&r.CreatedAt, &r.UpdatedAt, &r.DeletedAt)
	return r, err
}

// Create registers a repository.
func (s *RepositoryStore) Create(ctx context.Context, r domain.Repository) error {
	if err := r.Validate(); err != nil {
		return err
	}
	const q = `
		INSERT INTO repositories
			(id, user_id, full_name, local_path, device_id, default_branch, github_id, is_private)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	_, err := s.pool.Exec(ctx, q,
		r.ID, r.UserID, r.FullName, nullable(r.LocalPath), nullable(r.DeviceID),
		nullable(r.DefaultBranch), nullZero64(r.GitHubID), r.IsPrivate)
	return translateError(err, "repository")
}

// ByIDForUser loads a repository scoped to its owner.
func (s *RepositoryStore) ByIDForUser(ctx context.Context, repoID, userID string) (domain.Repository, error) {
	q := `SELECT ` + repoColumns + `
	      FROM repositories WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
	r, err := scanRepo(s.pool.QueryRow(ctx, q, repoID, userID))
	return r, translateError(err, "repository")
}

// ListForUser returns a user's repositories.
func (s *RepositoryStore) ListForUser(ctx context.Context, userID string) ([]domain.Repository, error) {
	q := `SELECT ` + repoColumns + `
	      FROM repositories WHERE user_id = $1 AND deleted_at IS NULL
	      ORDER BY full_name ASC`

	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, translateError(err, "repository")
	}
	defer rows.Close()

	out := make([]domain.Repository, 0, 8)
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, translateError(err, "repository")
		}
		out = append(out, r)
	}
	return out, translateError(rows.Err(), "repository")
}

// Update persists repository changes.
func (s *RepositoryStore) Update(ctx context.Context, r domain.Repository) error {
	if err := r.Validate(); err != nil {
		return err
	}
	const q = `
		UPDATE repositories
		SET local_path = $3, device_id = $4, default_branch = $5, is_private = $6
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`

	tag, err := s.pool.Exec(ctx, q,
		r.ID, r.UserID, nullable(r.LocalPath), nullable(r.DeviceID),
		nullable(r.DefaultBranch), r.IsPrivate)
	if err != nil {
		return translateError(err, "repository")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SoftDelete removes a repository from the user's list.
func (s *RepositoryStore) SoftDelete(ctx context.Context, repoID, userID string, at time.Time) error {
	const q = `
		UPDATE repositories SET deleted_at = $3
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
	tag, err := s.pool.Exec(ctx, q, repoID, userID, at)
	if err != nil {
		return translateError(err, "repository")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// nullZero64 maps 0 to NULL for optional bigint columns.
func nullZero64(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}
