// Package http exposes Beuvian's REST API over Fiber.
//
// This layer translates HTTP into use-case calls and domain errors into status
// codes. It holds no business logic: anything that would still be true if the
// transport were gRPC belongs in internal/app.
package http

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v2"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
)

// ErrorBody is the single error shape for every failure.
//
// One shape means a client needs one parser. `Code` is stable and machine-readable;
// `Message` is for humans and may change, which is why clients must branch on the
// code and never on the text.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries the failure itself.
type ErrorDetail struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// Stable error codes. Part of the API contract: a code may be added, but never
// renamed or repurposed.
const (
	codeBadRequest     = "bad_request"
	codeUnauthorized   = "unauthorized"
	codeForbidden      = "forbidden"
	codeNotFound       = "not_found"
	codeConflict       = "conflict"
	codeValidation     = "validation_failed"
	codeRateLimited    = "rate_limited"
	codeInternal       = "internal_error"
	codeTokenExpired   = "token_expired"
	codeDeviceOffline  = "device_offline"
	codeDeviceRevoked  = "device_revoked"
	codeAdapterMissing = "adapter_unavailable"
)

// writeError maps a domain error to an HTTP response.
//
// Centralised so no handler invents its own status conventions, and so the
// mapping from domain vocabulary to HTTP is stated once and reviewable in one
// place.
func writeError(c *fiber.Ctx, log *slog.Logger, err error) error {
	status, code, message := classify(err)

	requestID := blog.RequestIDFrom(c.UserContext())

	// 5xx is our fault and the detail could disclose internals, so the real error
	// goes to the log and the client gets a generic message with the request ID to
	// quote. 4xx is the caller's fault and the detail is exactly what they need.
	if status >= fiber.StatusInternalServerError {
		log.Error("request failed",
			slog.String("path", c.Path()),
			slog.String("method", c.Method()),
			slog.Int("status", status),
			blog.Err(err))
		message = "an internal error occurred"
	} else {
		log.Debug("request rejected",
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.String("code", code),
			blog.Err(err))
	}

	body := ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	}}

	// Field-level detail for validation failures, so a client can highlight the
	// offending input rather than showing a generic message.
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		body.Error.Details = map[string]any{"field": ve.Field}
	}

	return c.Status(status).JSON(body)
}

// classify maps an error to (status, code, message).
func classify(err error) (int, string, string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// Also returned for resources owned by another user. A 403 would confirm
		// the ID exists and let an attacker enumerate valid identifiers.
		return fiber.StatusNotFound, codeNotFound, "not found"

	case errors.Is(err, domain.ErrTokenExpired):
		// Distinguished from a plain 401 because the client action differs: an
		// expired token should trigger a refresh, an invalid one a fresh login.
		return fiber.StatusUnauthorized, codeTokenExpired, "credentials have expired"

	case errors.Is(err, domain.ErrUnauthorized):
		return fiber.StatusUnauthorized, codeUnauthorized, "authentication required"

	case errors.Is(err, domain.ErrForbidden):
		return fiber.StatusForbidden, codeForbidden, "not permitted"

	case errors.Is(err, domain.ErrDeviceRevoked):
		return fiber.StatusForbidden, codeDeviceRevoked, "this device has been revoked"

	case errors.Is(err, domain.ErrConflict):
		return fiber.StatusConflict, codeConflict, err.Error()

	case errors.Is(err, domain.ErrAdapterUnavailable):
		return fiber.StatusUnprocessableEntity, codeAdapterMissing, err.Error()

	case errors.Is(err, domain.ErrValidation):
		return fiber.StatusUnprocessableEntity, codeValidation, err.Error()

	case errors.Is(err, domain.ErrDeviceOffline):
		// 409, not 503: the server is fine, the requested device is not reachable.
		// Note this never applies to queueing a prompt, which succeeds regardless.
		return fiber.StatusConflict, codeDeviceOffline, "the device is not connected"

	case errors.Is(err, domain.ErrRateLimited):
		return fiber.StatusTooManyRequests, codeRateLimited, "rate limit exceeded"

	default:
		var fe *fiber.Error
		if errors.As(err, &fe) {
			return fe.Code, fiberCode(fe.Code), fe.Message
		}
		return fiber.StatusInternalServerError, codeInternal, err.Error()
	}
}

// fiberCode maps a bare HTTP status from Fiber to one of our stable codes.
func fiberCode(status int) string {
	switch status {
	case fiber.StatusBadRequest:
		return codeBadRequest
	case fiber.StatusUnauthorized:
		return codeUnauthorized
	case fiber.StatusForbidden:
		return codeForbidden
	case fiber.StatusNotFound:
		return codeNotFound
	case fiber.StatusConflict:
		return codeConflict
	case fiber.StatusTooManyRequests:
		return codeRateLimited
	default:
		return codeInternal
	}
}

// badRequest builds a 400 for a malformed request body or parameter.
func badRequest(message string) error {
	return fiber.NewError(fiber.StatusBadRequest, message)
}
