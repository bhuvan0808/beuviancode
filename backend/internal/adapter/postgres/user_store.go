package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
)

// UserStore implements port.UserStore.
type UserStore struct{ pool *pgxpool.Pool }

// NewUserStore returns a UserStore.
func NewUserStore(pool *pgxpool.Pool) *UserStore { return &UserStore{pool: pool} }

// Compile-time check that the adapter satisfies the port. Placed here rather than
// relying on the wiring in main to fail, so a broken implementation is a build
// error in this package.
var _ port.UserStore = (*UserStore)(nil)

// UpsertByGitHub creates or updates a user and their OAuth link atomically.
//
// One transaction rather than find-then-create because login is a single logical
// operation: two concurrent logins for a new user would otherwise both see "no
// row" and both insert, and the second would fail on the unique constraint after
// the first had already been handed a session.
//
// Settings are created here too, so every user has a settings row from the moment
// they exist and no read path has to handle absence.
func (s *UserStore) UpsertByGitHub(ctx context.Context, u domain.User, acct domain.OAuthAccount) (domain.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.User{}, translateError(err, "user")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ON CONFLICT on github_id, not github_login: logins are renameable, and a
	// rename must update the existing account rather than create a new one.
	const upsertUser = `
		INSERT INTO users (id, github_id, github_login, email, name, avatar_url, last_login_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (github_id) DO UPDATE SET
			github_login  = EXCLUDED.github_login,
			email         = COALESCE(NULLIF(EXCLUDED.email, ''), users.email),
			name          = COALESCE(NULLIF(EXCLUDED.name, ''), users.name),
			avatar_url    = EXCLUDED.avatar_url,
			last_login_at = EXCLUDED.last_login_at,
			-- Logging in un-deletes an account. Chosen deliberately: a returning
			-- user expects their devices and history back, and refusing would
			-- strand them with no way to recover.
			deleted_at    = NULL
		RETURNING id, github_id, github_login, COALESCE(email, ''), COALESCE(name, ''),
		          COALESCE(avatar_url, ''), COALESCE(organization_id, ''),
		          last_login_at, created_at, updated_at`

	var out domain.User
	var lastLogin *time.Time
	err = tx.QueryRow(ctx, upsertUser,
		u.ID, u.GitHubID, u.GitHubLogin, u.Email, u.Name, u.AvatarURL, u.LastLoginAt,
	).Scan(&out.ID, &out.GitHubID, &out.GitHubLogin, &out.Email, &out.Name,
		&out.AvatarURL, &out.OrganizationID, &lastLogin, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return domain.User{}, translateError(err, "user")
	}
	if lastLogin != nil {
		out.LastLoginAt = *lastLogin
	}

	const upsertAccount = `
		INSERT INTO oauth_accounts (id, user_id, provider, provider_user_id, access_token_encrypted, scopes)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (provider, provider_user_id) DO UPDATE SET
			user_id                = EXCLUDED.user_id,
			access_token_encrypted = EXCLUDED.access_token_encrypted,
			scopes                 = EXCLUDED.scopes`

	// A nil Go slice marshals to SQL NULL, and scopes is NOT NULL. Coerced here,
	// at the boundary, rather than expecting every caller to remember: the array
	// columns in this schema all mean "empty" rather than "unknown", so nil and
	// empty are the same thing to us.
	scopes := acct.Scopes
	if scopes == nil {
		scopes = []string{}
	}

	if _, err := tx.Exec(ctx, upsertAccount,
		acct.ID, out.ID, string(acct.Provider), acct.ProviderUserID,
		acct.AccessTokenEncrypted, scopes,
	); err != nil {
		return domain.User{}, translateError(err, "oauth_account")
	}

	// DO NOTHING so an existing user's customised settings are never reset by a
	// later login.
	const ensureSettings = `
		INSERT INTO user_settings (user_id) VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING`
	if _, err := tx.Exec(ctx, ensureSettings, out.ID); err != nil {
		return domain.User{}, translateError(err, "user_settings")
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.User{}, translateError(err, "user")
	}
	return out, nil
}

// ByID loads a user, excluding soft-deleted accounts.
func (s *UserStore) ByID(ctx context.Context, userID string) (domain.User, error) {
	const q = `
		SELECT id, github_id, github_login, COALESCE(email, ''), COALESCE(name, ''),
		       COALESCE(avatar_url, ''), COALESCE(organization_id, ''),
		       last_login_at, created_at, updated_at
		FROM users
		WHERE id = $1 AND deleted_at IS NULL`

	var u domain.User
	var lastLogin *time.Time
	err := s.pool.QueryRow(ctx, q, userID).Scan(
		&u.ID, &u.GitHubID, &u.GitHubLogin, &u.Email, &u.Name,
		&u.AvatarURL, &u.OrganizationID, &lastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return domain.User{}, translateError(err, "user")
	}
	if lastLogin != nil {
		u.LastLoginAt = *lastLogin
	}
	return u, nil
}

// Settings loads a user's preferences.
//
// Returns defaults rather than an error if the row is somehow missing. A user who
// cannot load settings would be unable to use the dashboard at all, which is a
// far worse outcome than silently using sensible defaults.
func (s *UserStore) Settings(ctx context.Context, userID string) (domain.UserSettings, error) {
	const q = `
		SELECT user_id, notify_on_complete, notify_on_waiting, notify_on_device_offline,
		       theme, timezone, log_retention_days, created_at, updated_at
		FROM user_settings WHERE user_id = $1`

	var st domain.UserSettings
	err := s.pool.QueryRow(ctx, q, userID).Scan(
		&st.UserID, &st.NotifyOnComplete, &st.NotifyOnWaiting, &st.NotifyOnDeviceOffline,
		&st.Theme, &st.Timezone, &st.LogRetentionDays, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return domain.DefaultSettings(userID), nil
		}
		return domain.UserSettings{}, translateError(err, "user_settings")
	}
	return st, nil
}

// SaveSettings persists preferences, creating the row if absent.
func (s *UserStore) SaveSettings(ctx context.Context, st domain.UserSettings) error {
	if err := st.Validate(); err != nil {
		return err
	}
	const q = `
		INSERT INTO user_settings
			(user_id, notify_on_complete, notify_on_waiting, notify_on_device_offline,
			 theme, timezone, log_retention_days)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id) DO UPDATE SET
			notify_on_complete       = EXCLUDED.notify_on_complete,
			notify_on_waiting        = EXCLUDED.notify_on_waiting,
			notify_on_device_offline = EXCLUDED.notify_on_device_offline,
			theme                    = EXCLUDED.theme,
			timezone                 = EXCLUDED.timezone,
			log_retention_days       = EXCLUDED.log_retention_days`

	_, err := s.pool.Exec(ctx, q,
		st.UserID, st.NotifyOnComplete, st.NotifyOnWaiting, st.NotifyOnDeviceOffline,
		st.Theme, st.Timezone, st.LogRetentionDays)
	return translateError(err, "user_settings")
}

// OAuthAccount loads a user's provider link, including the encrypted token.
func (s *UserStore) OAuthAccount(ctx context.Context, userID string, p domain.OAuthProvider) (domain.OAuthAccount, error) {
	const q = `
		SELECT id, user_id, provider, provider_user_id,
		       COALESCE(access_token_encrypted, ''), scopes, created_at, updated_at
		FROM oauth_accounts
		WHERE user_id = $1 AND provider = $2`

	var a domain.OAuthAccount
	var provider string
	err := s.pool.QueryRow(ctx, q, userID, string(p)).Scan(
		&a.ID, &a.UserID, &provider, &a.ProviderUserID,
		&a.AccessTokenEncrypted, &a.Scopes, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return domain.OAuthAccount{}, translateError(err, "oauth_account")
	}
	a.Provider = domain.OAuthProvider(provider)
	return a, nil
}

// AuditStore implements port.AuditLogger.
type AuditStore struct {
	pool *pgxpool.Pool
	log  logger
}

// logger is the minimal logging surface the audit store needs. Declared narrowly
// so this file does not depend on the whole slog API surface.
type logger interface {
	Error(msg string, args ...any)
}

// NewAuditStore returns an AuditStore.
func NewAuditStore(pool *pgxpool.Pool, log logger) *AuditStore {
	return &AuditStore{pool: pool, log: log}
}

var _ port.AuditLogger = (*AuditStore)(nil)

// Record writes an audit entry.
//
// Never returns an error, by design. An audit write that could fail a user's
// request would make auditing a liability rather than a safeguard — the correct
// response to "we could not record this" is to log loudly, not to reject the
// action the user already performed.
func (s *AuditStore) Record(ctx context.Context, e domain.AuditEntry) {
	const q = `
		INSERT INTO audit_log
			(user_id, action, target_type, target_id, metadata, ip_address, user_agent, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	var userID *string
	if e.UserID != "" {
		userID = &e.UserID
	}

	// Detach from the request context: an audit entry for a cancelled request is
	// exactly the one worth keeping, and inheriting the cancellation would drop it.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if _, err := s.pool.Exec(writeCtx, q,
		userID, e.Action, nullable(e.TargetType), nullable(e.TargetID),
		e.Metadata, nullable(e.IPAddress), nullable(e.UserAgent), nullable(e.CorrelationID),
	); err != nil {
		s.log.Error("audit write failed",
			"action", e.Action, "target_id", e.TargetID, "error", err.Error())
	}
}

// nullable maps an empty string to SQL NULL, so optional columns hold NULL rather
// than empty strings. Partial indexes with `WHERE col IS NOT NULL` depend on this.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// NewID mints a prefixed identifier. Implements port.IDGenerator.
type IDGen struct{}

// NewID returns a prefixed ULID.
func (IDGen) NewID(prefix string) string { return id.WithPrefix(prefix) }

var _ port.IDGenerator = IDGen{}

// SystemClock implements port.Clock against the wall clock.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

var _ port.Clock = SystemClock{}
