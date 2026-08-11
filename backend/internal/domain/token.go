package domain

import "time"

// TokenKind distinguishes the credential families.
//
// A leaked device token must not grant dashboard access, and a leaked browser
// session must not silently control every machine. Encoding the kind in the token
// and checking it on every use is what keeps the two separate — without it, a
// device token would be accepted anywhere a user token is.
type TokenKind string

const (
	// KindAccess is a short-lived dashboard access token.
	KindAccess TokenKind = "access"
	// KindDevice is a long-lived per-device agent token.
	KindDevice TokenKind = "device"
)

// Claims is the payload carried by an issued JWT.
//
// Deliberately minimal. Every field here is replicated into every request's
// token and cannot be revoked before expiry, so anything that might change — a
// display name, a permission set — belongs in the database instead.
type Claims struct {
	Subject   string // user ID
	Kind      TokenKind
	DeviceID  string // set only for KindDevice
	IssuedAt  time.Time
	ExpiresAt time.Time
	// TokenID identifies this specific token, for audit correlation.
	TokenID string
}

// Valid reports whether the claims are structurally usable at now.
func (c *Claims) Valid(now time.Time) error {
	if c.Subject == "" {
		return ErrUnauthorized
	}
	if c.Kind == KindDevice && c.DeviceID == "" {
		// A device token with no device cannot be checked against revocation,
		// which would make revocation unenforceable.
		return ErrUnauthorized
	}
	if now.After(c.ExpiresAt) {
		return ErrTokenExpired
	}
	return nil
}

// RefreshToken is a long-lived, revocable credential stored hashed.
type RefreshToken struct {
	ID     string
	UserID string

	// TokenHash is SHA-256 of the plaintext, which exists only in the client's
	// cookie. A database dump therefore cannot be replayed as a login.
	TokenHash string

	// FamilyID groups a rotation lineage. Rotation issues a new token in the same
	// family; presenting an already-used token revokes the entire family.
	FamilyID string

	ExpiresAt time.Time

	// UsedAt is set when the token is rotated. A second presentation of a token
	// with UsedAt set means it was stolen — both the attacker and the legitimate
	// user now hold it — so ending every session in the family is the correct
	// response rather than an over-reaction.
	UsedAt *time.Time

	UserAgent string
	IPAddress string

	CreatedAt time.Time
}

// Usable reports whether the token can be exchanged at now.
func (t *RefreshToken) Usable(now time.Time) error {
	if t.UsedAt != nil {
		return ErrTokenReused
	}
	if now.After(t.ExpiresAt) {
		return ErrTokenExpired
	}
	return nil
}

// OAuthState is the CSRF guard for the authorization code flow.
//
// Stored server-side in Redis with a short TTL rather than being a signed
// cookie, so it can be consumed exactly once — a replayed callback finds the
// state already gone.
type OAuthState struct {
	State string
	// Redirect is where to send the user after login, validated against an
	// allowlist before use so the flow cannot be turned into an open redirect.
	Redirect  string
	CreatedAt time.Time
}
