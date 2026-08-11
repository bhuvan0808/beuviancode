package domain

import "errors"

// Domain errors.
//
// These are the vocabulary the application layer speaks in. Adapters translate
// them into their own transport's terms — internal/adapter/http maps ErrNotFound
// to 404, internal/adapter/ws maps it to a protocol ERROR frame. A use case that
// returned an HTTP status directly could not be reused over WebSocket, which is
// exactly the coupling this indirection removes.
//
// Compared with errors.Is, never by string.
var (
	// ErrNotFound means the requested entity does not exist.
	//
	// Deliberately also returned when an entity exists but belongs to another
	// user. Distinguishing "forbidden" from "not found" confirms an ID is real,
	// which lets an attacker enumerate valid identifiers.
	ErrNotFound = errors.New("domain: not found")

	// ErrForbidden means the caller is authenticated but not permitted. Used for
	// action-level denials where existence is not a secret.
	ErrForbidden = errors.New("domain: forbidden")

	// ErrUnauthorized means no valid credential was presented.
	ErrUnauthorized = errors.New("domain: unauthorized")

	// ErrConflict means the operation contradicts current state — starting a
	// second session on a device that already has one running.
	ErrConflict = errors.New("domain: conflict")

	// ErrValidation means the input is well-formed but semantically invalid.
	ErrValidation = errors.New("domain: validation failed")

	// ErrTokenExpired is separated from ErrUnauthorized because the client
	// response differs: an expired token should trigger a refresh, while an
	// invalid one should trigger a new login.
	ErrTokenExpired = errors.New("domain: token expired")

	// ErrTokenReused means an already-rotated refresh token was presented. This
	// is treated as theft, not as a mistake: the whole token family is revoked.
	ErrTokenReused = errors.New("domain: refresh token reuse detected")

	// ErrDeviceOffline means the target device has no live connection. Note that
	// this is NOT an error when queueing a prompt — prompts are durable and
	// delivered on reconnect. It matters only for operations that require an
	// immediate round trip, such as stopping a session.
	ErrDeviceOffline = errors.New("domain: device is offline")

	// ErrDeviceRevoked means the device's credentials were revoked.
	ErrDeviceRevoked = errors.New("domain: device revoked")

	// ErrAdapterUnavailable means the device does not have the requested coding
	// agent installed. Checked before starting a session so the user learns
	// immediately rather than after walking away from the machine.
	ErrAdapterUnavailable = errors.New("domain: coding agent not available on device")

	// ErrRateLimited means the caller exceeded their quota.
	ErrRateLimited = errors.New("domain: rate limited")
)

// ValidationError carries field-level detail for a failed validation.
//
// Implements Unwrap so errors.Is(err, ErrValidation) succeeds, letting the HTTP
// adapter map it to 422 generically while still surfacing which field was wrong.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return "domain: " + e.Field + ": " + e.Message
}

func (e *ValidationError) Unwrap() error { return ErrValidation }

// Invalid builds a ValidationError.
func Invalid(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}
