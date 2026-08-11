// Package auth implements token issuance, verification, and the GitHub OAuth flow.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
)

// issuer is the `iss` claim, and is verified on every token.
//
// Prevents a token minted by some other system that happens to share our signing
// secret from being accepted here — the situation that arises when a secret is
// reused across environments by mistake.
const issuer = "beuvian"

// TokenService implements port.TokenIssuer and port.TokenVerifier.
type TokenService struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	deviceTTL  time.Duration
}

// NewTokenService builds a TokenService from auth configuration.
func NewTokenService(cfg config.Auth) *TokenService {
	return &TokenService{
		secret:     []byte(cfg.JWTSecret),
		accessTTL:  cfg.AccessTokenTTL,
		refreshTTL: cfg.RefreshTokenTTL,
		deviceTTL:  cfg.DeviceTokenTTL,
	}
}

var (
	_ port.TokenIssuer   = (*TokenService)(nil)
	_ port.TokenVerifier = (*TokenService)(nil)
)

// claims is the JWT payload.
//
// Kind is what keeps the two credential families apart: a device token presented
// to a dashboard endpoint must be rejected even though it is validly signed.
// Without this field, a leaked device token would grant full account access.
type claims struct {
	jwt.RegisteredClaims
	Kind     domain.TokenKind `json:"kind"`
	DeviceID string           `json:"device_id,omitempty"`
}

// IssueAccess mints a short-lived dashboard token.
//
// Short-lived because access tokens are not revocable without a database lookup on
// every request. Expiry is what bounds the damage from a leak, so the TTL is a
// security control rather than a tuning knob.
func (s *TokenService) IssueAccess(userID string) (string, domain.Claims, error) {
	return s.issue(userID, domain.KindAccess, "", s.accessTTL)
}

// IssueDevice mints a long-lived per-device agent token.
//
// Scoped to one device so a leak cannot impersonate the user's browser session,
// and long-lived because an unattended agent cannot re-run an interactive login.
// Revocability comes from the devices table, not from expiry.
func (s *TokenService) IssueDevice(userID, deviceID string) (string, domain.Claims, error) {
	if deviceID == "" {
		return "", domain.Claims{}, errors.New("auth: device token requires a device id")
	}
	return s.issue(userID, domain.KindDevice, deviceID, s.deviceTTL)
}

func (s *TokenService) issue(userID string, kind domain.TokenKind, deviceID string, ttl time.Duration) (string, domain.Claims, error) {
	if len(s.secret) == 0 {
		return "", domain.Claims{}, errors.New("auth: signing secret is not configured")
	}

	now := time.Now().UTC()
	tokenID := id.WithPrefix("tok")
	expires := now.Add(ttl)

	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    issuer,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
		Kind:     kind,
		DeviceID: deviceID,
	}

	// HS256: symmetric, and appropriate because the only verifier is this same
	// service. Asymmetric signing would matter if a third party needed to verify
	// tokens without being able to mint them, which is not the case here.
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.secret)
	if err != nil {
		return "", domain.Claims{}, fmt.Errorf("auth: sign token: %w", err)
	}

	return signed, domain.Claims{
		Subject:   userID,
		Kind:      kind,
		DeviceID:  deviceID,
		IssuedAt:  now,
		ExpiresAt: expires,
		TokenID:   tokenID,
	}, nil
}

// Verify checks a token's signature, issuer, and expiry.
//
// It deliberately does NOT check revocation: that requires a database lookup and
// belongs to the caller, which keeps this pure, cheap, and usable on the hot path
// of every request.
func (s *TokenService) Verify(token string) (domain.Claims, error) {
	if len(s.secret) == 0 {
		return domain.Claims{}, errors.New("auth: signing secret is not configured")
	}

	var c claims
	parsed, err := jwt.ParseWithClaims(token, &c,
		func(t *jwt.Token) (any, error) {
			// Pinning the algorithm is essential. Without it a token with
			// alg=none, or an HMAC token verified against a public key, can be
			// forged — the classic JWT vulnerability.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
			}
			return s.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		// Expiry is separated from other failures because the client response
		// differs: an expired token should trigger a refresh, an invalid one a
		// fresh login.
		if errors.Is(err, jwt.ErrTokenExpired) {
			return domain.Claims{}, domain.ErrTokenExpired
		}
		return domain.Claims{}, fmt.Errorf("%w: %v", domain.ErrUnauthorized, err)
	}
	if !parsed.Valid {
		return domain.Claims{}, domain.ErrUnauthorized
	}

	out := domain.Claims{
		Subject:  c.Subject,
		Kind:     c.Kind,
		DeviceID: c.DeviceID,
		TokenID:  c.ID,
	}
	if c.IssuedAt != nil {
		out.IssuedAt = c.IssuedAt.Time
	}
	if c.ExpiresAt != nil {
		out.ExpiresAt = c.ExpiresAt.Time
	}

	// Structural checks the JWT library cannot make for us — notably that a device
	// token actually names a device, without which revocation is unenforceable.
	if err := out.Valid(time.Now().UTC()); err != nil {
		return domain.Claims{}, err
	}
	return out, nil
}

// refreshTokenBytes is the entropy in a refresh token.
//
// 32 bytes (256 bits) makes guessing infeasible. Refresh tokens are long-lived
// and directly exchangeable for a session, so they warrant more entropy than a
// short-lived identifier.
const refreshTokenBytes = 32

// IssueRefresh mints a refresh token.
//
// Returns the plaintext for the cookie and the hash for storage from a single
// call, so no caller can accidentally persist the plaintext — the mistake that
// would make a database dump replayable as a set of logins.
func (s *TokenService) IssueRefresh(userID, familyID string) (string, string, time.Time, error) {
	if userID == "" || familyID == "" {
		return "", "", time.Time{}, errors.New("auth: refresh token requires a user and family")
	}

	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", time.Time{}, fmt.Errorf("auth: generate refresh token: %w", err)
	}
	// URL-safe and unpadded so it survives a cookie round trip without escaping.
	plaintext := base64.RawURLEncoding.EncodeToString(buf)

	return plaintext, s.HashRefresh(plaintext), time.Now().UTC().Add(s.refreshTTL), nil
}

// HashRefresh computes the storage hash of a refresh token.
//
// Plain SHA-256 rather than bcrypt or argon2, which is the right call here and is
// worth stating explicitly: those are for low-entropy human passwords, where slow
// hashing defeats brute force. A refresh token is 256 bits of CSPRNG output, so
// there is nothing to brute-force, and a slow hash on every refresh would only add
// latency to a hot path.
func (s *TokenService) HashRefresh(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// HashDeviceToken computes the storage hash of a device token.
//
// Same reasoning as HashRefresh: the input is a signed JWT, not a password.
func HashDeviceToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ConstantTimeEqual compares two hashes without leaking timing.
//
// Used when checking a presented device token against the stored hash. A naive
// == returns early at the first differing byte, which is measurable and lets an
// attacker recover the value one byte at a time.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// NewFamilyID mints a refresh-token family identifier.
func NewFamilyID() string { return id.WithPrefix("fam") }

// NewState mints an OAuth state value for CSRF protection.
func NewState() string { return id.Nonce() }
