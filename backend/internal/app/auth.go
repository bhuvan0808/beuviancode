package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
)

// AuthService handles login, token refresh, and logout.
type AuthService struct {
	users    port.UserStore
	tokens   port.RefreshTokenStore
	issuer   port.TokenIssuer
	verifier port.TokenVerifier
	oauth    port.OAuthProvider
	cache    port.Cache
	audit    port.AuditLogger
	ids      port.IDGenerator
	clock    port.Clock
	log      *slog.Logger

	stateTTL time.Duration
}

// AuthDeps groups AuthService's collaborators.
//
// A struct rather than a long positional parameter list: with nine dependencies,
// positional arguments are trivially transposable at the call site and the
// compiler cannot catch it when two share a type.
type AuthDeps struct {
	Users    port.UserStore
	Tokens   port.RefreshTokenStore
	Issuer   port.TokenIssuer
	Verifier port.TokenVerifier
	OAuth    port.OAuthProvider
	Cache    port.Cache
	Audit    port.AuditLogger
	IDs      port.IDGenerator
	Clock    port.Clock
	Log      *slog.Logger
	StateTTL time.Duration
}

// NewAuthService builds an AuthService.
func NewAuthService(d AuthDeps) *AuthService {
	return &AuthService{
		users: d.Users, tokens: d.Tokens, issuer: d.Issuer, verifier: d.Verifier,
		oauth: d.OAuth, cache: d.Cache, audit: d.Audit, ids: d.IDs, clock: d.Clock,
		log:      d.Log.With(slog.String("service", "auth")),
		stateTTL: d.StateTTL,
	}
}

// stateCachePrefix namespaces OAuth state entries.
const stateCachePrefix = "oauth:state:"

// BeginLogin starts the GitHub authorization code flow.
//
// The state parameter is stored server-side rather than being a signed cookie, so
// it can be consumed exactly once: a replayed callback finds it already gone. A
// stateless signed value would remain valid for its whole lifetime and could be
// replayed within that window.
func (s *AuthService) BeginLogin(ctx context.Context, redirect string) (string, error) {
	state := domain.OAuthState{
		State:     newState(),
		Redirect:  redirect,
		CreatedAt: s.clock.Now(),
	}

	body, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("auth: encode state: %w", err)
	}
	if err := s.cache.Set(ctx, stateCachePrefix+state.State, body, s.stateTTL); err != nil {
		// Without a stored state the callback cannot be validated, so this must
		// fail rather than proceeding with CSRF protection silently disabled.
		return "", fmt.Errorf("auth: store oauth state: %w", err)
	}
	return s.oauth.AuthCodeURL(state.State), nil
}

// LoginResult carries the credentials minted by a successful login.
type LoginResult struct {
	User             domain.User
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	Redirect         string
}

// CompleteLogin finishes the OAuth flow and issues Beuvian credentials.
func (s *AuthService) CompleteLogin(ctx context.Context, code, state, userAgent, ipAddress string) (LoginResult, error) {
	// Validate state FIRST, before spending a GitHub round trip on the code. An
	// attacker-supplied callback should cost us as little as possible.
	stored, err := s.consumeState(ctx, state)
	if err != nil {
		return LoginResult{}, err
	}

	accessToken, err := s.oauth.Exchange(ctx, code)
	if err != nil {
		return LoginResult{}, fmt.Errorf("%w: %v", domain.ErrUnauthorized, err)
	}

	ghUser, err := s.oauth.CurrentUser(ctx, accessToken)
	if err != nil {
		return LoginResult{}, fmt.Errorf("%w: %v", domain.ErrUnauthorized, err)
	}

	now := s.clock.Now()
	user := domain.User{
		ID:          s.ids.NewID(id.PrefixUser),
		GitHubID:    ghUser.ID,
		GitHubLogin: ghUser.Login,
		Email:       ghUser.Email,
		Name:        ghUser.Name,
		AvatarURL:   ghUser.AvatarURL,
		LastLoginAt: now,
	}
	account := domain.OAuthAccount{
		ID:             s.ids.NewID("oap"),
		Provider:       domain.ProviderGitHub,
		ProviderUserID: fmt.Sprint(ghUser.ID),
		// TODO(phase-7): encrypt at rest with a KMS-held key. Stored as-is for now
		// and used only for read-only repository metadata; tracked as a known gap
		// rather than left silent.
		AccessTokenEncrypted: accessToken,
		Scopes:               []string{"read:user", "user:email", "repo"},
	}

	saved, err := s.users.UpsertByGitHub(ctx, user, account)
	if err != nil {
		return LoginResult{}, err
	}

	result, err := s.issueSession(ctx, saved, newFamilyID(), userAgent, ipAddress)
	if err != nil {
		return LoginResult{}, err
	}
	result.Redirect = stored.Redirect

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: saved.ID, Action: domain.ActionUserLogin,
		TargetType: "user", TargetID: saved.ID,
		IPAddress: ipAddress, UserAgent: userAgent,
		CorrelationID: blog.CorrelationIDFrom(ctx),
		CreatedAt:     now,
	})
	s.log.Info("user logged in",
		slog.String("user_id", saved.ID), slog.String("github_login", saved.GitHubLogin))

	return result, nil
}

// consumeState validates and removes a one-time OAuth state.
func (s *AuthService) consumeState(ctx context.Context, state string) (domain.OAuthState, error) {
	if state == "" {
		return domain.OAuthState{}, fmt.Errorf("%w: missing oauth state", domain.ErrUnauthorized)
	}

	body, err := s.cache.Get(ctx, stateCachePrefix+state)
	if err != nil {
		// A missing state means expired, already used, or forged. All three are
		// indistinguishable to us and all are rejected identically.
		return domain.OAuthState{}, fmt.Errorf("%w: invalid or expired oauth state", domain.ErrUnauthorized)
	}
	// Delete immediately so a concurrent replay of the same callback fails.
	_ = s.cache.Delete(ctx, stateCachePrefix+state)

	var stored domain.OAuthState
	if err := json.Unmarshal(body, &stored); err != nil {
		return domain.OAuthState{}, fmt.Errorf("%w: corrupt oauth state", domain.ErrUnauthorized)
	}
	return stored, nil
}

// Refresh rotates a refresh token and issues a new access token.
//
// Implements rotation with reuse detection. Each refresh marks the presented token
// used and issues a replacement in the same family; presenting an already-used
// token means two parties hold it, so the entire family is revoked. Treating reuse
// as theft rather than as a client bug is the whole point — a stolen token is
// otherwise indistinguishable from a legitimate one.
func (s *AuthService) Refresh(ctx context.Context, plaintext, userAgent, ipAddress string) (LoginResult, error) {
	if plaintext == "" {
		return LoginResult{}, domain.ErrUnauthorized
	}

	hash := s.verifier.HashRefresh(plaintext)
	stored, err := s.tokens.ByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return LoginResult{}, domain.ErrUnauthorized
		}
		return LoginResult{}, err
	}

	now := s.clock.Now()
	if err := stored.Usable(now); err != nil {
		if errors.Is(err, domain.ErrTokenReused) {
			// Revoke every session in the lineage. The legitimate user is logged
			// out too, which is the correct trade: being logged out is a nuisance,
			// an attacker holding a valid session is not.
			if rerr := s.tokens.RevokeFamily(ctx, stored.FamilyID); rerr != nil {
				s.log.Error("failed to revoke token family after reuse",
					slog.String("family_id", stored.FamilyID), blog.Err(rerr))
			}
			s.audit.Record(ctx, domain.AuditEntry{
				UserID: stored.UserID, Action: domain.ActionTokenReuse,
				TargetType: "refresh_token_family", TargetID: stored.FamilyID,
				IPAddress: ipAddress, UserAgent: userAgent,
				CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: now,
			})
			s.log.Warn("refresh token reuse detected; family revoked",
				slog.String("user_id", stored.UserID),
				slog.String("family_id", stored.FamilyID),
				slog.String("ip", ipAddress))
		}
		return LoginResult{}, domain.ErrUnauthorized
	}

	// Mark used before issuing the replacement. If the process dies between the
	// two, the user must re-login — inconvenient, but strictly safer than the
	// reverse ordering, which would leave two live tokens in the family.
	if err := s.tokens.MarkUsed(ctx, stored.ID, now); err != nil {
		if errors.Is(err, domain.ErrTokenReused) {
			// Lost a race with a concurrent refresh of the same token.
			_ = s.tokens.RevokeFamily(ctx, stored.FamilyID)
		}
		return LoginResult{}, domain.ErrUnauthorized
	}

	user, err := s.users.ByID(ctx, stored.UserID)
	if err != nil {
		return LoginResult{}, domain.ErrUnauthorized
	}

	result, err := s.issueSession(ctx, user, stored.FamilyID, userAgent, ipAddress)
	if err != nil {
		return LoginResult{}, err
	}

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: user.ID, Action: domain.ActionTokenRefreshed,
		TargetType: "refresh_token_family", TargetID: stored.FamilyID,
		IPAddress: ipAddress, UserAgent: userAgent,
		CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: now,
	})
	return result, nil
}

// issueSession mints an access token and a refresh token in one family.
func (s *AuthService) issueSession(ctx context.Context, user domain.User, familyID, userAgent, ipAddress string) (LoginResult, error) {
	accessToken, claims, err := s.issuer.IssueAccess(user.ID)
	if err != nil {
		return LoginResult{}, err
	}

	refreshPlain, refreshHash, refreshExpires, err := s.issuer.IssueRefresh(user.ID, familyID)
	if err != nil {
		return LoginResult{}, err
	}

	record := domain.RefreshToken{
		ID:        s.ids.NewID("rft"),
		UserID:    user.ID,
		TokenHash: refreshHash,
		FamilyID:  familyID,
		ExpiresAt: refreshExpires,
		UserAgent: truncate(userAgent, 512),
		IPAddress: ipAddress,
		CreatedAt: s.clock.Now(),
	}
	if err := s.tokens.Create(ctx, record); err != nil {
		return LoginResult{}, err
	}

	return LoginResult{
		User:             user,
		AccessToken:      accessToken,
		AccessExpiresAt:  claims.ExpiresAt,
		RefreshToken:     refreshPlain,
		RefreshExpiresAt: refreshExpires,
	}, nil
}

// Logout revokes the presented refresh token's family.
//
// Revokes the family rather than the single token so logging out on one device
// does not leave a rotation lineage alive that could still be refreshed.
func (s *AuthService) Logout(ctx context.Context, plaintext string) error {
	if plaintext == "" {
		return nil // already logged out; not an error worth surfacing
	}
	stored, err := s.tokens.ByHash(ctx, s.verifier.HashRefresh(plaintext))
	if err != nil {
		// An unknown token means the session is already gone. Reporting an error
		// would leave a client unable to complete logout.
		return nil
	}
	if err := s.tokens.RevokeFamily(ctx, stored.FamilyID); err != nil {
		return err
	}
	s.audit.Record(ctx, domain.AuditEntry{
		UserID: stored.UserID, Action: domain.ActionUserLogout,
		TargetType: "refresh_token_family", TargetID: stored.FamilyID,
		CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: s.clock.Now(),
	})
	return nil
}

// Me loads the authenticated user.
func (s *AuthService) Me(ctx context.Context, userID string) (domain.User, error) {
	return s.users.ByID(ctx, userID)
}

// VerifyAccess validates a dashboard access token.
//
// Rejects device tokens explicitly. Both are validly signed by the same secret, so
// without this check a leaked agent credential would grant full dashboard access —
// which is precisely the separation the two token kinds exist to maintain.
func (s *AuthService) VerifyAccess(token string) (domain.Claims, error) {
	claims, err := s.verifier.Verify(token)
	if err != nil {
		return domain.Claims{}, err
	}
	if claims.Kind != domain.KindAccess {
		return domain.Claims{}, fmt.Errorf("%w: wrong token kind", domain.ErrUnauthorized)
	}
	return claims, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
