package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	blog "github.com/bhuvan0808/beuviancode/shared/log"
)

// decode parses the single JSON record written to buf.
func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no log output was produced")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output is not valid JSON (%v): %s", err, line)
	}
	return rec
}

func TestJSONFormatIncludesRequiredFields(t *testing.T) {
	var buf bytes.Buffer
	l := blog.New(blog.Config{Level: "info", Format: blog.FormatJSON, Component: "backend"}, &buf)

	l.Info("session started",
		slog.String(blog.FieldSessionID, "ses_01J9Z3K7QF8XKM2N4P6R8T0VWY"),
		slog.String(blog.FieldDeviceID, "dev_01J9Z3K7QF8XKM2N4P6R8T0VWY"),
		slog.String(blog.FieldCorrelationID, "cor_01J9Z3K7QF8XKM2N4P6R8T0VWY"),
		slog.String(blog.FieldRequestID, "req_01J9Z3K7QF8XKM2N4P6R8T0VWY"),
	)

	rec := decode(t, &buf)
	// PROJECT.md requires all of these on a structured record.
	for _, key := range []string{
		slog.TimeKey, slog.LevelKey, slog.MessageKey,
		blog.FieldSessionID, blog.FieldDeviceID,
		blog.FieldCorrelationID, blog.FieldRequestID, blog.FieldComponent,
	} {
		if _, ok := rec[key]; !ok {
			t.Errorf("record is missing required field %q: %v", key, rec)
		}
	}
	if rec[blog.FieldComponent] != "backend" {
		t.Errorf("component = %v, want backend", rec[blog.FieldComponent])
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	// Defence in depth: code should not log credentials, but a careless future
	// call site must not be able to turn a log sink into a credential store.
	secret := "ghp_supersecrettokenvalue"
	cases := []string{
		"token", "access_token", "refresh_token", "Authorization",
		"password", "client_secret", "api_key", "APIKey",
		"cookie", "private_key", "db_credential", "passphrase",
	}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			l := blog.New(blog.Config{Level: "info", Format: blog.FormatJSON}, &buf)
			l.Info("handshake", slog.String(key, secret))

			if strings.Contains(buf.String(), secret) {
				t.Errorf("secret leaked for key %q: %s", key, buf.String())
			}
			if got := decode(t, &buf)[key]; got != "[REDACTED]" {
				t.Errorf("%q = %v, want [REDACTED]", key, got)
			}
		})
	}
}

func TestNonSensitiveFieldsSurvive(t *testing.T) {
	// The redactor must not be so eager that it destroys useful diagnostics.
	var buf bytes.Buffer
	l := blog.New(blog.Config{Level: "info", Format: blog.FormatJSON}, &buf)
	l.Info("status", slog.String("repository", "beuvian/beuvian"), slog.Int("queued_prompts", 3))

	rec := decode(t, &buf)
	if rec["repository"] != "beuvian/beuvian" {
		t.Errorf("repository was altered: %v", rec["repository"])
	}
	if rec["queued_prompts"] != float64(3) {
		t.Errorf("queued_prompts = %v, want 3", rec["queued_prompts"])
	}
}

func TestErrorsRenderAsMessages(t *testing.T) {
	var buf bytes.Buffer
	l := blog.New(blog.Config{Level: "error", Format: blog.FormatJSON}, &buf)
	l.Error("dispatch failed", blog.Err(errors.New("redis: connection refused")))

	if got := decode(t, &buf)[blog.FieldError]; got != "redis: connection refused" {
		t.Errorf("error = %v, want the error message", got)
	}
}

func TestLevelFilteringAndParsing(t *testing.T) {
	if blog.ParseLevel("debug") != slog.LevelDebug {
		t.Error("debug should parse to LevelDebug")
	}
	if blog.ParseLevel("WARNING") != slog.LevelWarn {
		t.Error("level parsing should be case-insensitive and accept 'warning'")
	}
	// An unrecognised level must degrade to info, not kill the process:
	// losing observability is worse than an unknown level string.
	if blog.ParseLevel("verbose") != slog.LevelInfo {
		t.Error("unknown levels should fall back to LevelInfo")
	}

	var buf bytes.Buffer
	l := blog.New(blog.Config{Level: "warn", Format: blog.FormatJSON}, &buf)
	l.Debug("noisy")
	l.Info("chatty")
	if buf.Len() != 0 {
		t.Errorf("records below the configured level were emitted: %s", buf.String())
	}
	l.Warn("important")
	if buf.Len() == 0 {
		t.Error("warn records should be emitted at level=warn")
	}
}

func TestContextPropagation(t *testing.T) {
	ctx := blog.WithCorrelationID(context.Background(), "cor_abc")
	ctx = blog.WithRequestID(ctx, "req_xyz")

	if got := blog.CorrelationIDFrom(ctx); got != "cor_abc" {
		t.Errorf("CorrelationIDFrom = %q", got)
	}
	if got := blog.RequestIDFrom(ctx); got != "req_xyz" {
		t.Errorf("RequestIDFrom = %q", got)
	}

	var buf bytes.Buffer
	base := blog.New(blog.Config{Level: "info", Format: blog.FormatJSON}, &buf)
	blog.Enrich(ctx, base).Info("forwarding prompt")

	rec := decode(t, &buf)
	if rec[blog.FieldCorrelationID] != "cor_abc" {
		t.Errorf("correlation_id = %v", rec[blog.FieldCorrelationID])
	}
	if rec[blog.FieldRequestID] != "req_xyz" {
		t.Errorf("request_id = %v", rec[blog.FieldRequestID])
	}
}

func TestFromContextNeverReturnsNil(t *testing.T) {
	// A logging call must never be the thing that panics a request.
	if blog.FromContext(context.Background()) == nil {
		t.Fatal("FromContext returned nil for a bare context")
	}
	//nolint:staticcheck // deliberately exercising the nil-context path
	if blog.FromContext(nil) == nil {
		t.Fatal("FromContext returned nil for a nil context")
	}
	blog.FromContext(context.Background()).Info("must not panic")

	var buf bytes.Buffer
	want := blog.New(blog.Config{Level: "info", Format: blog.FormatJSON}, &buf)
	ctx := blog.IntoContext(context.Background(), want)
	blog.FromContext(ctx).Info("round trip")
	if buf.Len() == 0 {
		t.Error("IntoContext/FromContext did not round-trip the logger")
	}
}

func TestEnrichWithoutContextFieldsIsANoOp(t *testing.T) {
	var buf bytes.Buffer
	base := blog.New(blog.Config{Level: "info", Format: blog.FormatJSON}, &buf)
	if got := blog.Enrich(context.Background(), base); got != base {
		t.Error("Enrich should return the same logger when the context carries no fields")
	}
	if blog.Enrich(context.Background(), nil) == nil {
		t.Error("Enrich(nil logger) must not return nil")
	}
}
