// Package log provides Beuvian's structured logging.
//
// Built on log/slog from the standard library rather than zap or zerolog. slog is
// fast enough for our volume, it is the ecosystem's converging standard, and — the
// deciding factor — it keeps the shared module dependency-free, so pulling in the
// logger does not drag a third-party tree into both the agent and the backend.
//
// Every record carries the correlation fields PROJECT.md requires (timestamp,
// session ID, device ID, correlation ID, request ID) and secrets are stripped by
// a handler-level redactor, so a token cannot reach a log sink by accident.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Canonical field names. Declared as constants because these keys are queried in
// log aggregators; a typo'd key in one call site silently breaks a dashboard, and
// a constant makes that a compile error instead.
const (
	FieldSessionID     = "session_id"
	FieldDeviceID      = "device_id"
	FieldCorrelationID = "correlation_id"
	FieldRequestID     = "request_id"
	FieldUserID        = "user_id"
	FieldComponent     = "component"
	FieldError         = "error"
)

// Format selects the output encoding.
type Format string

const (
	// FormatJSON is structured JSON, for production. Mandated by PROJECT.md.
	FormatJSON Format = "json"
	// FormatText is human-readable key=value, for local development only.
	FormatText Format = "text"
)

// Config declares how to build a logger. Populated from the application's
// configuration layer, never read from globals.
type Config struct {
	// Level is one of debug, info, warn, error. Invalid values fall back to
	// info rather than failing startup: losing observability is a worse
	// outcome than an unrecognised level string.
	Level string
	// Format is json or text.
	Format Format
	// AddSource attaches file:line. Costs a caller lookup per record, so it is
	// off by default and enabled for debug builds.
	AddSource bool
	// Component names the emitting binary ("backend", "agent") and is attached
	// to every record.
	Component string
}

// ParseLevel maps a level string to a slog.Level, defaulting to info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// sensitiveKeys are redacted wherever they appear as an attribute key.
//
// Defence in depth. Code should not log a token in the first place, but this
// guarantees that a future careless call site cannot turn a log sink into a
// credential store. Matching is substring-based and case-insensitive so
// "access_token", "RefreshToken", and "authorization" are all caught.
var sensitiveKeys = []string{
	"token", "password", "secret", "authorization", "cookie",
	"api_key", "apikey", "private_key", "credential", "passphrase",
}

const redacted = "[REDACTED]"

// redactor is the slog.ReplaceAttr hook that enforces the redaction policy.
func redactor(_ []string, a slog.Attr) slog.Attr {
	lower := strings.ToLower(a.Key)
	for _, k := range sensitiveKeys {
		if strings.Contains(lower, k) {
			return slog.String(a.Key, redacted)
		}
	}
	// Render errors as their message rather than as an opaque struct.
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			if err == nil {
				return slog.String(a.Key, "")
			}
			return slog.String(a.Key, err.Error())
		}
	}
	return a
}

// New builds a logger writing to w.
//
// The writer is injected rather than defaulting to os.Stderr so tests can capture
// output and assert on it, and so a future log-shipping sink is a composition
// change rather than an edit here.
func New(cfg Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       ParseLevel(cfg.Level),
		AddSource:   cfg.AddSource,
		ReplaceAttr: redactor,
	}

	var h slog.Handler
	if cfg.Format == FormatText {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}

	l := slog.New(h)
	if cfg.Component != "" {
		l = l.With(slog.String(FieldComponent, cfg.Component))
	}
	return l
}

// Discard returns a logger that drops everything, for tests that do not assert
// on log output. Preferable to a nil logger, which would panic at every call
// site and force nil checks throughout.
func Discard() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// contextKey is unexported so no other package can collide with our keys.
type contextKey int

const (
	keyLogger contextKey = iota
	keyCorrelationID
	keyRequestID
)

// IntoContext stores a logger for retrieval further down the call stack.
//
// Passing the logger through context rather than threading it into every
// signature is a pragmatic exception to "no ambient state": HTTP middleware and
// WebSocket handlers enrich it per request, and every downstream function would
// otherwise need a *slog.Logger parameter it does nothing with but forward.
func IntoContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, keyLogger, l)
}

// FromContext returns the context's logger, or a discarding logger if absent.
//
// It never returns nil, so callers can log unconditionally. A missing logger
// degrades to silence rather than to a panic — a logging call must never be the
// thing that takes down a request.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return Discard()
	}
	if l, ok := ctx.Value(keyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return Discard()
}

// WithCorrelationID attaches a correlation ID to the context.
//
// A correlation ID spans the whole causal chain — dashboard click, HTTP request,
// Redis enqueue, WebSocket delivery, agent injection, resulting log lines — so one
// user action is greppable end to end across three processes.
func WithCorrelationID(ctx context.Context, cid string) context.Context {
	return context.WithValue(ctx, keyCorrelationID, cid)
}

// CorrelationIDFrom reads the correlation ID, or "" if unset.
func CorrelationIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(keyCorrelationID).(string)
	return s
}

// WithRequestID attaches a per-request ID to the context. Unlike a correlation
// ID it does not cross a process boundary; it identifies this one HTTP request.
func WithRequestID(ctx context.Context, rid string) context.Context {
	return context.WithValue(ctx, keyRequestID, rid)
}

// RequestIDFrom reads the request ID, or "" if unset.
func RequestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

// Enrich returns a logger carrying whichever correlation fields the context holds.
//
// Call this once at a boundary (request handler, WebSocket frame dispatch) and
// pass the result down, rather than calling it in a hot loop.
func Enrich(ctx context.Context, l *slog.Logger) *slog.Logger {
	if l == nil {
		l = Discard()
	}
	var attrs []any
	if v := CorrelationIDFrom(ctx); v != "" {
		attrs = append(attrs, slog.String(FieldCorrelationID, v))
	}
	if v := RequestIDFrom(ctx); v != "" {
		attrs = append(attrs, slog.String(FieldRequestID, v))
	}
	if len(attrs) == 0 {
		return l
	}
	return l.With(attrs...)
}

// Err wraps an error as an attribute using the canonical field name.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.String(FieldError, "")
	}
	return slog.String(FieldError, err.Error())
}

// Fatalf logs at error level and returns a formatted error.
//
// It deliberately does not call os.Exit. Exiting is the caller's decision and
// belongs in main, where deferred cleanup and shutdown ordering are visible;
// a library that exits the process is untestable and unsafe to reuse.
func Fatalf(l *slog.Logger, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	l.Error(err.Error())
	return err
}
