package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// minProductionSecretLength is the floor for the JWT signing secret.
//
// 32 bytes matches the HMAC-SHA256 output size. A shorter secret reduces the
// effective key space below the hash's strength, making offline brute force
// against a captured token feasible.
const minProductionSecretLength = 32

// Validate checks the resolved configuration.
//
// Every problem is collected and reported together rather than failing on the
// first one. A fresh deployment typically has several settings missing at once,
// and one-at-a-time errors turn that into a slow guessing game.
func (c *Config) Validate() error {
	var errs []error

	switch c.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("env: %q is not one of development, staging, production", c.Env))
	}

	errs = append(errs, c.Server.validate()...)
	errs = append(errs, c.Database.validate(c.Env)...)
	errs = append(errs, c.Redis.validate()...)
	errs = append(errs, c.Auth.validate(c.Env)...)
	errs = append(errs, c.CORS.validate(c.Env)...)
	errs = append(errs, c.RateLimit.validate()...)
	errs = append(errs, c.WebSocket.validate()...)
	errs = append(errs, c.Log.validate()...)

	errs = append(errs, c.crossFieldChecks()...)

	if len(errs) > 0 {
		return fmt.Errorf("invalid backend configuration:\n%w", errors.Join(errs...))
	}
	return nil
}

// crossFieldChecks covers rules that span sections, which is where the genuinely
// dangerous misconfigurations live: each field looks fine alone.
func (c *Config) crossFieldChecks() []error {
	var errs []error

	if c.Env.IsProduction() {
		// An insecure cookie in production means the refresh token travels in
		// cleartext on any downgraded request. Refuse rather than silently
		// "fixing" it, so the deployment config gets corrected at its source.
		if !c.Auth.CookieSecure {
			errs = append(errs, errors.New("auth.cookie_secure: must be true in production"))
		}
		for _, o := range c.CORS.AllowedOrigins {
			if strings.HasPrefix(normalizeOrigin(o), "http://") {
				errs = append(errs, fmt.Errorf(
					"cors.allowed_origins: %q is plaintext http; production requires https", o))
			}
		}
		if strings.HasPrefix(c.Auth.DashboardURL, "http://") {
			errs = append(errs, fmt.Errorf(
				"auth.dashboard_url: %q is plaintext http; production requires https", c.Auth.DashboardURL))
		}
		if c.Log.Level == "debug" {
			// Debug logging in production is a data-exposure risk as much as a
			// volume problem.
			errs = append(errs, errors.New("log.level: debug is not permitted in production"))
		}
	}

	// Credentialed CORS with a wildcard origin is rejected by browsers anyway;
	// catching it here turns a baffling runtime failure into a boot-time message.
	if c.CORS.AllowCredentials {
		for _, o := range c.CORS.AllowedOrigins {
			if o == "*" {
				errs = append(errs, errors.New(
					"cors: allow_credentials with a wildcard origin is rejected by browsers; list origins explicitly"))
			}
		}
	}

	// A refresh token shorter-lived than an access token inverts the whole
	// design: the session would end before the access token needed renewing.
	if c.Auth.RefreshTokenTTL <= c.Auth.AccessTokenTTL {
		errs = append(errs, fmt.Errorf(
			"auth: refresh_token_ttl (%s) must exceed access_token_ttl (%s)",
			c.Auth.RefreshTokenTTL, c.Auth.AccessTokenTTL))
	}

	// Rate limiting is enforced in Redis. Enabled without Redis silently means
	// "not enforced" — a security control that looks on but is off.
	//
	// Outside development this is a hard error. In development it is not: a fresh
	// clone must run with zero setup, and requiring a Redis instance before
	// `go run ./cmd/server` works would be a needless barrier. main warns loudly
	// in that case instead, so the degradation is visible rather than silent.
	if c.RateLimit.Enabled && c.Redis.URL == "" && !c.Env.IsDevelopment() {
		errs = append(errs, errors.New(
			"rate_limit.enabled requires redis.url; without Redis the limiter cannot enforce anything"))
	}

	if c.Database.MaxIdleConns > c.Database.MaxOpenConns {
		errs = append(errs, fmt.Errorf(
			"database: max_idle_conns (%d) cannot exceed max_open_conns (%d)",
			c.Database.MaxIdleConns, c.Database.MaxOpenConns))
	}

	return errs
}

func (s Server) validate() []error {
	var errs []error
	if s.Port < 1 || s.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.port: %d is outside 1..65535", s.Port))
	}
	if s.Host == "" {
		errs = append(errs, errors.New("server.host: must not be empty"))
	}
	if s.BodyLimit < 1024 {
		errs = append(errs, fmt.Errorf("server.body_limit: %d is too small to carry a request", s.BodyLimit))
	}
	if s.ShutdownGrace <= 0 {
		errs = append(errs, errors.New("server.shutdown_grace: must be positive"))
	}
	// Railway sends SIGKILL 30s after SIGTERM; a longer grace would be truncated
	// mid-drain, which is worse than a shorter one that completes.
	if s.ShutdownGrace > 30*time.Second {
		errs = append(errs, fmt.Errorf(
			"server.shutdown_grace: %s exceeds the platform's 30s SIGTERM window", s.ShutdownGrace))
	}
	for _, p := range s.TrustedProxies {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, errors.New("server.trusted_proxies: contains an empty entry"))
		}
	}
	return errs
}

func (d Database) validate(env Environment) []error {
	var errs []error
	if d.URL == "" {
		// Development can start without a database so `--help` and config
		// checks work on a clean clone; production cannot.
		if env != EnvDevelopment {
			errs = append(errs, errors.New("database.url: required outside development"))
		}
	} else if err := validateDSN(d.URL, "database.url", "postgres", "postgresql"); err != nil {
		errs = append(errs, err)
	}
	if d.MaxOpenConns < 1 {
		errs = append(errs, fmt.Errorf("database.max_open_conns: %d must be at least 1", d.MaxOpenConns))
	}
	if d.MaxIdleConns < 0 {
		errs = append(errs, errors.New("database.max_idle_conns: must not be negative"))
	}
	if d.ConnectTimeout <= 0 {
		errs = append(errs, errors.New("database.connect_timeout: must be positive"))
	}
	return errs
}

func (r Redis) validate() []error {
	var errs []error
	if r.URL != "" {
		if err := validateDSN(r.URL, "redis.url", "redis", "rediss"); err != nil {
			errs = append(errs, err)
		}
	} else if r.Required {
		errs = append(errs, errors.New("redis.required is set but redis.url is empty"))
	}
	if r.PoolSize < 1 {
		errs = append(errs, fmt.Errorf("redis.pool_size: %d must be at least 1", r.PoolSize))
	}
	if r.KeyPrefix == "" {
		// Without a prefix, staging and production sharing one Upstash instance
		// would read and flush each other's keys.
		errs = append(errs, errors.New("redis.key_prefix: must not be empty"))
	}
	return errs
}

func (a Auth) validate(env Environment) []error {
	var errs []error

	required := env != EnvDevelopment
	if a.JWTSecret == "" {
		if required {
			errs = append(errs, errors.New("auth.jwt_secret: required outside development"))
		}
	} else if len(a.JWTSecret) < minProductionSecretLength {
		msg := fmt.Errorf("auth.jwt_secret: %d bytes is below the %d-byte minimum for HS256",
			len(a.JWTSecret), minProductionSecretLength)
		if required {
			errs = append(errs, msg)
		}
		// In development a short secret is tolerated; the startup log warns.
	}

	if required {
		if a.GitHubClientID == "" {
			errs = append(errs, errors.New("auth.github_client_id: required outside development"))
		}
		if a.GitHubClientSecret == "" {
			errs = append(errs, errors.New("auth.github_client_secret: required outside development"))
		}
		if a.GitHubCallbackURL == "" {
			errs = append(errs, errors.New("auth.github_callback_url: required outside development"))
		}
	}

	if a.GitHubCallbackURL != "" {
		if err := validateHTTPURL(a.GitHubCallbackURL, "auth.github_callback_url"); err != nil {
			errs = append(errs, err)
		}
	}
	if a.DashboardURL != "" {
		if err := validateHTTPURL(a.DashboardURL, "auth.dashboard_url"); err != nil {
			errs = append(errs, err)
		}
	}

	if a.AccessTokenTTL <= 0 {
		errs = append(errs, errors.New("auth.access_token_ttl: must be positive"))
	}
	if a.RefreshTokenTTL <= 0 {
		errs = append(errs, errors.New("auth.refresh_token_ttl: must be positive"))
	}
	if a.DeviceTokenTTL <= 0 {
		errs = append(errs, errors.New("auth.device_token_ttl: must be positive"))
	}
	if a.StateTTL <= 0 {
		errs = append(errs, errors.New("auth.state_ttl: must be positive"))
	}
	return errs
}

func (c CORS) validate(env Environment) []error {
	var errs []error
	if len(c.AllowedOrigins) == 0 {
		errs = append(errs, errors.New("cors.allowed_origins: at least one origin is required or the dashboard cannot call the API"))
	}
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			if env.IsProduction() {
				errs = append(errs, errors.New("cors.allowed_origins: a wildcard is not permitted in production"))
			}
			continue
		}
		if err := validateHTTPURL(normalizeOrigin(o), "cors.allowed_origins"); err != nil {
			errs = append(errs, err)
		}
	}
	if len(c.AllowedMethods) == 0 {
		errs = append(errs, errors.New("cors.allowed_methods: must not be empty"))
	}
	return errs
}

func (r RateLimit) validate() []error {
	var errs []error
	if !r.Enabled {
		return errs
	}
	if r.Requests < 1 {
		errs = append(errs, fmt.Errorf("rate_limit.requests: %d must be at least 1", r.Requests))
	}
	if r.Window <= 0 {
		errs = append(errs, errors.New("rate_limit.window: must be positive"))
	}
	if r.LoginRequests < 1 {
		errs = append(errs, fmt.Errorf("rate_limit.login_requests: %d must be at least 1", r.LoginRequests))
	}
	if r.LoginWindow <= 0 {
		errs = append(errs, errors.New("rate_limit.login_window: must be positive"))
	}
	// A login budget looser than the general one makes the tighter limit
	// pointless — the endpoint worth brute-forcing would be the least protected.
	if r.LoginRequests > r.Requests {
		errs = append(errs, fmt.Errorf(
			"rate_limit: login_requests (%d) should not exceed requests (%d)", r.LoginRequests, r.Requests))
	}
	return errs
}

func (w WebSocket) validate() []error {
	var errs []error
	if w.MaxConnectionsPerUser < 1 {
		errs = append(errs, fmt.Errorf("websocket.max_connections_per_user: %d must be at least 1", w.MaxConnectionsPerUser))
	}
	if w.ReadBufferSize < 512 {
		errs = append(errs, fmt.Errorf("websocket.read_buffer_size: %d is impractically small", w.ReadBufferSize))
	}
	if w.WriteBufferSize < 512 {
		errs = append(errs, fmt.Errorf("websocket.write_buffer_size: %d is impractically small", w.WriteBufferSize))
	}
	if w.SendQueueSize < 1 {
		errs = append(errs, fmt.Errorf("websocket.send_queue_size: %d must be at least 1", w.SendQueueSize))
	}
	if w.HandshakeTimeout <= 0 {
		errs = append(errs, errors.New("websocket.handshake_timeout: must be positive"))
	}
	return errs
}

func (l Log) validate() []error {
	var errs []error
	switch strings.ToLower(l.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level: %q is not one of debug, info, warn, error", l.Level))
	}
	switch strings.ToLower(l.Format) {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format: %q is not one of json, text", l.Format))
	}
	return errs
}

// validateDSN checks that a connection string parses and uses an expected scheme.
//
// Deliberately does not attempt to connect: validation must be pure and fast so
// it can run in CI and in a `--check` invocation with no network.
func validateDSN(raw, field string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: not a valid URL: %w", field, err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("%s: missing scheme (expected one of %s)", field, strings.Join(schemes, ", "))
	}
	for _, s := range schemes {
		if u.Scheme == s {
			if u.Host == "" {
				return fmt.Errorf("%s: missing host", field)
			}
			return nil
		}
	}
	return fmt.Errorf("%s: scheme %q is not one of %s", field, u.Scheme, strings.Join(schemes, ", "))
}

func validateHTTPURL(raw, field string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: not a valid URL: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: scheme %q is not http or https (%q)", field, u.Scheme, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: missing host in %q", field, raw)
	}
	return nil
}
