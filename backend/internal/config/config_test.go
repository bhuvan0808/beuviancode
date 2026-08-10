package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/config"
)

// loadDev returns a valid development configuration, as a base for mutation.
//
// Using Load rather than a hand-built literal is deliberate: it exercises the real
// default tags, so a test cannot pass against defaults that the binary would never
// actually produce.
func loadDev(t *testing.T, args ...string) *config.Config {
	t.Helper()
	cfg, _, _, err := config.Load(args)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestCleanCloneStartsWithNoConfiguration(t *testing.T) {
	// The most important property for contributor experience: `go run ./cmd/server`
	// must work on a fresh clone with no file, no environment, and no flags.
	cfg, ops, file, err := config.Load(nil)
	if err != nil {
		t.Fatalf("a clean clone must produce a valid configuration, got: %v", err)
	}
	if file != "" {
		t.Errorf("no config file should have been found, got %q", file)
	}
	if ops.Check || ops.Version {
		t.Error("operational flags should default to false")
	}
	if !cfg.Env.IsDevelopment() {
		t.Errorf("Env = %q, want development", cfg.Env)
	}
	if cfg.Server.Addr() != "0.0.0.0:8080" {
		t.Errorf("Addr = %q, want 0.0.0.0:8080", cfg.Server.Addr())
	}
}

func TestRateLimitWithoutRedisIsToleratedOnlyInDevelopment(t *testing.T) {
	// Regression guard. Rate limiting defaults to enabled and Redis defaults to
	// empty, so a strict cross-field check made a clean clone fail to start.
	// Development must degrade with a warning; anything else must refuse.
	dev := loadDev(t)
	if !dev.RateLimit.Enabled || dev.Redis.URL != "" {
		t.Fatal("test premise broken: expected rate limiting on and Redis empty by default")
	}
	if err := dev.Validate(); err != nil {
		t.Errorf("development should tolerate rate limiting without Redis: %v", err)
	}

	staging := loadDev(t)
	staging.Env = config.EnvStaging
	// Supply the other production-tier requirements so only the Redis rule can fail.
	staging.Database.URL = "postgres://u:p@db.example.com:5432/beuvian"
	staging.Auth.JWTSecret = strings.Repeat("k", 32)
	staging.Auth.GitHubClientID = "id"
	staging.Auth.GitHubClientSecret = "secret"
	staging.Auth.GitHubCallbackURL = "https://api.example.com/v1/auth/github/callback"

	err := staging.Validate()
	if err == nil {
		t.Fatal("staging must reject rate limiting with no Redis")
	}
	if !strings.Contains(err.Error(), "rate_limit.enabled requires redis.url") {
		t.Errorf("error should name the rate-limit/Redis conflict, got: %v", err)
	}
}

// production returns a configuration that is valid at the production tier, so each
// subtest can break exactly one thing.
func production(t *testing.T) *config.Config {
	t.Helper()
	cfg := loadDev(t)
	cfg.Env = config.EnvProduction
	cfg.Database.URL = "postgres://u:p@db.pooler.supabase.com:6543/postgres?sslmode=require"
	cfg.Redis.URL = "rediss://default:p@eu.upstash.io:6379"
	cfg.Auth.JWTSecret = strings.Repeat("k", 32)
	cfg.Auth.GitHubClientID = "client-id"
	cfg.Auth.GitHubClientSecret = "client-secret"
	cfg.Auth.GitHubCallbackURL = "https://api.example.com/v1/auth/github/callback"
	cfg.Auth.DashboardURL = "https://app.example.com"
	cfg.CORS.AllowedOrigins = []string{"https://app.example.com"}
	return cfg
}

func TestProductionBaselineIsValid(t *testing.T) {
	// If this fails, every negative test below is meaningless.
	if err := production(t).Validate(); err != nil {
		t.Fatalf("the production baseline should be valid: %v", err)
	}
}

func TestProductionRejectsSecurityDefects(t *testing.T) {
	// Each of these would otherwise run silently in production for months. They are
	// the reason validation is environment-aware rather than uniform.
	tests := []struct {
		name    string
		break_  func(*config.Config)
		wantMsg string
	}{
		{
			"insecure cookie",
			func(c *config.Config) { c.Auth.CookieSecure = false },
			"cookie_secure",
		},
		{
			"plaintext CORS origin",
			func(c *config.Config) { c.CORS.AllowedOrigins = []string{"http://app.example.com"} },
			"plaintext http",
		},
		{
			"wildcard CORS origin",
			func(c *config.Config) { c.CORS.AllowedOrigins = []string{"*"} },
			"wildcard",
		},
		{
			"plaintext dashboard URL",
			func(c *config.Config) { c.Auth.DashboardURL = "http://app.example.com" },
			"plaintext http",
		},
		{
			"debug logging",
			func(c *config.Config) { c.Log.Level = "debug" },
			"debug is not permitted in production",
		},
		{
			"short signing secret",
			func(c *config.Config) { c.Auth.JWTSecret = "too-short" },
			"below the 32-byte minimum",
		},
		{
			"missing database",
			func(c *config.Config) { c.Database.URL = "" },
			"database.url",
		},
		{
			"missing GitHub client id",
			func(c *config.Config) { c.Auth.GitHubClientID = "" },
			"github_client_id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := production(t)
			tc.break_(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("production must reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error should mention %q, got: %v", tc.wantMsg, err)
			}
		})
	}
}

func TestDevelopmentToleratesWhatProductionRejects(t *testing.T) {
	// The mirror of the test above. A fresh clone must not need TLS, a database, or
	// GitHub credentials — otherwise nobody can run the project locally.
	cfg := loadDev(t)
	cfg.Log.Level = "debug"
	cfg.Auth.JWTSecret = "short"
	cfg.CORS.AllowedOrigins = []string{"http://localhost:3000"}

	if err := cfg.Validate(); err != nil {
		t.Errorf("development should tolerate local-only settings: %v", err)
	}
}

func TestValidationAggregatesEveryProblem(t *testing.T) {
	// A fresh deployment usually has several settings missing at once. Reporting one
	// at a time turns that into a slow guessing game.
	cfg := loadDev(t)
	cfg.Env = config.EnvProduction
	// Everything is wrong: no database, no secret, no GitHub app, http origins.
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	msg := err.Error()
	for _, want := range []string{
		"database.url", "jwt_secret", "github_client_id",
		"github_client_secret", "github_callback_url", "plaintext http",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q:\n%s", want, msg)
		}
	}
	if n := strings.Count(msg, "\n"); n < 5 {
		t.Errorf("expected several problems reported together, got %d lines:\n%s", n+1, msg)
	}
}

func TestCrossFieldInvariants(t *testing.T) {
	tests := []struct {
		name    string
		break_  func(*config.Config)
		wantMsg string
	}{
		{
			// Inverts the whole token design: the session would end before the
			// access token ever needed renewing.
			"refresh TTL shorter than access TTL",
			func(c *config.Config) {
				c.Auth.AccessTokenTTL = time.Hour
				c.Auth.RefreshTokenTTL = time.Minute
			},
			"must exceed access_token_ttl",
		},
		{
			"idle conns exceed open conns",
			func(c *config.Config) {
				c.Database.MaxOpenConns = 5
				c.Database.MaxIdleConns = 10
			},
			"cannot exceed max_open_conns",
		},
		{
			// A looser budget on the endpoint worth brute-forcing makes the
			// tighter general limit pointless.
			"login budget looser than the general budget",
			func(c *config.Config) {
				c.RateLimit.Requests = 10
				c.RateLimit.LoginRequests = 100
			},
			"should not exceed requests",
		},
		{
			// Railway sends SIGKILL 30s after SIGTERM, so a longer grace is
			// truncated mid-drain rather than honoured.
			"shutdown grace beyond the platform window",
			func(c *config.Config) { c.Server.ShutdownGrace = time.Minute },
			"exceeds the platform's 30s SIGTERM window",
		},
		{
			"credentialed CORS with a wildcard",
			func(c *config.Config) {
				c.Env = config.EnvDevelopment
				c.CORS.AllowCredentials = true
				c.CORS.AllowedOrigins = []string{"*"}
			},
			"rejected by browsers",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := production(t)
			tc.break_(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error should mention %q, got: %v", tc.wantMsg, err)
			}
		})
	}
}

func TestMalformedConnectionStringsAreRejected(t *testing.T) {
	tests := []struct{ name, dsn, wantMsg string }{
		// "localhost:5432/beuvian" parses with "localhost" AS the scheme, so it is
		// caught by the scheme check rather than the missing-scheme branch. Either
		// way it is rejected, and the message names the offending scheme.
		{"host mistaken for a scheme", "localhost:5432/beuvian", "is not one of"},
		{"no scheme at all", "//localhost:5432/beuvian", "missing scheme"},
		{"wrong scheme", "mysql://u:p@localhost:3306/beuvian", "is not one of"},
		{"missing host", "postgres:///beuvian", "missing host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := production(t)
			cfg.Database.URL = tc.dsn
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected %q to be rejected", tc.dsn)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error should mention %q, got: %v", tc.wantMsg, err)
			}
		})
	}

	// Redis accepts both redis:// and rediss:// — Upstash uses the TLS form.
	for _, ok := range []string{"redis://localhost:6379", "rediss://default:p@eu.upstash.io:6379"} {
		cfg := production(t)
		cfg.Redis.URL = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
}

func TestPlatformPortIsAdoptedButLosesToAnExplicitFlag(t *testing.T) {
	// Railway injects PORT, not BEUVIAN_SERVER_PORT. It is translated into our
	// namespace as an ENVIRONMENT-layer value, so precedence still holds and an
	// explicit flag wins. Applying it after resolution would silently beat the flag.
	t.Setenv("PORT", "5555")

	cfg := loadDev(t)
	if cfg.Server.Port != 5555 {
		t.Errorf("Port = %d, want the platform PORT to be adopted", cfg.Server.Port)
	}

	cfg = loadDev(t, "-port", "7777")
	if cfg.Server.Port != 7777 {
		t.Errorf("Port = %d, want the explicit flag to beat the platform PORT", cfg.Server.Port)
	}
}

func TestOurEnvVarBeatsThePlatformPort(t *testing.T) {
	t.Setenv("PORT", "5555")
	t.Setenv("BEUVIAN_SERVER_PORT", "6666")
	if cfg := loadDev(t); cfg.Server.Port != 6666 {
		t.Errorf("Port = %d, want our own variable to take precedence over PORT", cfg.Server.Port)
	}
}

func TestDescribeRedactsSecrets(t *testing.T) {
	cfg := production(t)
	lines, err := cfg.Describe()
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	joined := strings.Join(lines, "\n")

	for _, secret := range []string{cfg.Auth.JWTSecret, cfg.Auth.GitHubClientSecret, cfg.Database.URL, cfg.Redis.URL} {
		if strings.Contains(joined, secret) {
			t.Errorf("Describe leaked a secret value:\n%s", joined)
		}
	}
	// Non-secret values must still be visible or the dump is useless.
	if !strings.Contains(joined, "Server.Port=8080") {
		t.Errorf("expected non-secret values to be shown:\n%s", joined)
	}
}

func TestVersionFlagSkipsValidation(t *testing.T) {
	// -version must work on a machine with no valid configuration at all.
	t.Setenv("BEUVIAN_ENV", "production")
	cfg, ops, _, err := config.Load([]string{"-version"})
	if err != nil {
		t.Fatalf("-version should not require valid configuration: %v", err)
	}
	if !ops.Version {
		t.Error("ops.Version should be true")
	}
	if cfg == nil {
		t.Error("a config should still be returned for -version")
	}
}

func TestUnknownEnvironmentIsRejected(t *testing.T) {
	cfg := loadDev(t)
	cfg.Env = "prod" // a plausible typo for "production"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unrecognised environment name must be rejected")
	}
	if !strings.Contains(err.Error(), "not one of development, staging, production") {
		t.Errorf("error should list the valid tiers, got: %v", err)
	}
}
