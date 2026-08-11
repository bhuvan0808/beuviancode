// Package config declares and loads the backend's configuration.
//
// The struct is the schema: every setting is declared once, with its env name,
// CLI flag, default, and secrecy in tags beside it. The precedence engine in
// shared/config turns that declaration into the CLI > Env > File > Defaults
// resolution PROJECT.md requires, so this package holds no loading logic of its
// own — only the schema and its validation rules.
package config

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	sharedconfig "github.com/bhuvan0808/beuviancode/shared/config"
)

// EnvPrefix namespaces every environment variable. Railway and Docker inject a
// flat environment, so a prefix prevents collisions with platform variables.
const EnvPrefix = "BEUVIAN_"

// Environment names a deployment tier. Validation is stricter in production, so
// a misconfiguration fails at boot rather than becoming a security hole.
type Environment string

const (
	// EnvDevelopment is the local development tier: lenient validation.
	EnvDevelopment Environment = "development"
	// EnvStaging is the pre-production tier: production validation with a
	// permissive surface for testing.
	EnvStaging Environment = "staging"
	// EnvProduction is the live tier: validation rejects anything that would
	// weaken the deployment.
	EnvProduction Environment = "production"
)

// IsProduction reports whether the tier is production.
func (e Environment) IsProduction() bool { return e == EnvProduction }

// IsDevelopment reports whether the tier is development.
func (e Environment) IsDevelopment() bool { return e == EnvDevelopment }

// Config is the backend's complete configuration.
//
// Grouped into sections that mirror the adapters they configure, so an adapter
// receives only its own section rather than the whole struct. That keeps the
// dependency direction honest: the Postgres adapter cannot reach the GitHub OAuth
// secret, because it is never handed it.
type Config struct {
	Env       Environment `yaml:"env" env:"ENV" flag:"env" default:"development" usage:"deployment tier: development|staging|production"`
	Server    Server      `yaml:"server" env:"SERVER"`
	Database  Database    `yaml:"database" env:"DB"`
	Redis     Redis       `yaml:"redis" env:"REDIS"`
	Auth      Auth        `yaml:"auth" env:"AUTH"`
	CORS      CORS        `yaml:"cors" env:"CORS"`
	RateLimit RateLimit   `yaml:"rate_limit" env:"RATELIMIT"`
	WebSocket WebSocket   `yaml:"websocket" env:"WS"`
	Log       Log         `yaml:"log" env:"LOG"`
}

// Server configures the HTTP/WebSocket listener.
type Server struct {
	Host string `yaml:"host" env:"HOST" flag:"host" default:"0.0.0.0" usage:"bind address"`

	// Port defaults to 8080 but Railway injects PORT; see Load for how that is
	// reconciled without letting the platform silently override an explicit flag.
	Port int `yaml:"port" env:"PORT" flag:"port" default:"8080" usage:"listen port"`

	// ReadTimeout bounds how long reading a request may take. It must stay
	// generous enough not to kill WebSocket upgrades, which is why the gateway
	// sets its own deadlines rather than relying on this.
	ReadTimeout  time.Duration `yaml:"read_timeout" env:"READ_TIMEOUT" default:"15s"`
	WriteTimeout time.Duration `yaml:"write_timeout" env:"WRITE_TIMEOUT" default:"15s"`
	IdleTimeout  time.Duration `yaml:"idle_timeout" env:"IDLE_TIMEOUT" default:"120s"`

	// ShutdownGrace bounds graceful shutdown. Kept under Railway's 30s
	// SIGTERM-to-SIGKILL window so draining completes rather than being killed
	// halfway through.
	ShutdownGrace time.Duration `yaml:"shutdown_grace" env:"SHUTDOWN_GRACE" default:"15s"`

	// BodyLimit caps request bodies. Prompts are small; a large limit would only
	// widen the memory-exhaustion surface.
	BodyLimit int `yaml:"body_limit" env:"BODY_LIMIT" default:"1048576" usage:"max request body in bytes"`

	// TrustedProxies lists proxy CIDRs whose X-Forwarded-For may be believed.
	// Empty means trust none. This is a security control, not a convenience:
	// blindly trusting the header lets any client spoof its IP and defeat
	// per-IP rate limiting.
	TrustedProxies []string `yaml:"trusted_proxies" env:"TRUSTED_PROXIES"`
}

// Addr returns the host:port listen address.
func (s Server) Addr() string { return fmt.Sprintf("%s:%d", s.Host, s.Port) }

// Database configures Supabase PostgreSQL.
type Database struct {
	// URL is the full connection string. Held as a single URL rather than
	// discrete fields because that is what Supabase hands out, and splitting it
	// would invite transcription errors.
	URL string `yaml:"url" env:"URL" secret:"true" usage:"postgres connection URL"`

	// MaxOpenConns must account for Supabase's connection ceiling shared across
	// all instances. Set too high, a scale-up exhausts the database's limit and
	// every instance starts failing at once.
	MaxOpenConns    int           `yaml:"max_open_conns" env:"MAX_OPEN_CONNS" default:"25"`
	MaxIdleConns    int           `yaml:"max_idle_conns" env:"MAX_IDLE_CONNS" default:"5"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime" env:"CONN_MAX_LIFETIME" default:"30m"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time" env:"CONN_MAX_IDLE_TIME" default:"5m"`

	// ConnectTimeout bounds the initial connection so a wrong URL fails fast at
	// boot instead of hanging the health check.
	ConnectTimeout time.Duration `yaml:"connect_timeout" env:"CONNECT_TIMEOUT" default:"10s"`

	// AutoMigrate applies pending migrations at startup. Off by default:
	// migrating from application boot means N instances race to migrate during a
	// rolling deploy. Migrations run as an explicit CI step instead.
	AutoMigrate bool `yaml:"auto_migrate" env:"AUTO_MIGRATE" flag:"auto-migrate" default:"false"`
}

// Redis configures Upstash Redis.
type Redis struct {
	URL string `yaml:"url" env:"URL" secret:"true" usage:"redis connection URL (rediss://...)"`

	PoolSize     int           `yaml:"pool_size" env:"POOL_SIZE" default:"10"`
	DialTimeout  time.Duration `yaml:"dial_timeout" env:"DIAL_TIMEOUT" default:"5s"`
	ReadTimeout  time.Duration `yaml:"read_timeout" env:"READ_TIMEOUT" default:"3s"`
	WriteTimeout time.Duration `yaml:"write_timeout" env:"WRITE_TIMEOUT" default:"3s"`

	// KeyPrefix namespaces every key, so a shared Upstash instance can host
	// staging and production without one flushing the other.
	KeyPrefix string `yaml:"key_prefix" env:"KEY_PREFIX" default:"beuvian:"`

	// Required makes Redis a hard dependency. False by default so the backend
	// degrades rather than dies: with Redis down, prompts still persist to
	// Postgres and are delivered on reconnect. Losing hot dispatch is
	// recoverable; losing the API is not.
	Required bool `yaml:"required" env:"REQUIRED" default:"false"`
}

// Auth configures GitHub OAuth and token issuance.
type Auth struct {
	GitHubClientID     string `yaml:"github_client_id" env:"GITHUB_CLIENT_ID"`
	GitHubClientSecret string `yaml:"github_client_secret" env:"GITHUB_CLIENT_SECRET" secret:"true"`

	// GitHubCallbackURL must match the GitHub OAuth app exactly, including
	// scheme and trailing path.
	GitHubCallbackURL string `yaml:"github_callback_url" env:"GITHUB_CALLBACK_URL"`

	// JWTSecret signs access tokens. Validation enforces a minimum length in
	// production; a short secret makes HS256 brute-forceable offline.
	JWTSecret string `yaml:"jwt_secret" env:"JWT_SECRET" secret:"true"`

	// AccessTokenTTL is deliberately short. Access tokens are not revocable
	// without a lookup on every request, so their blast radius is bounded by
	// expiry instead; refresh tokens carry revocability.
	AccessTokenTTL time.Duration `yaml:"access_token_ttl" env:"ACCESS_TOKEN_TTL" default:"15m"`

	// RefreshTokenTTL is long-lived, stored hashed, and rotated on use.
	RefreshTokenTTL time.Duration `yaml:"refresh_token_ttl" env:"REFRESH_TOKEN_TTL" default:"720h"`

	// DeviceTokenTTL applies to agent tokens. Longer than a browser session
	// because an unattended desktop agent cannot re-run an interactive login.
	DeviceTokenTTL time.Duration `yaml:"device_token_ttl" env:"DEVICE_TOKEN_TTL" default:"2160h"`

	// CookieDomain scopes the refresh cookie. Empty means host-only, which is
	// correct unless the dashboard and API sit on different subdomains.
	CookieDomain string `yaml:"cookie_domain" env:"COOKIE_DOMAIN"`

	// CookieSecure forces the Secure attribute. Validation pins this to true in
	// production regardless of what was configured.
	CookieSecure bool `yaml:"cookie_secure" env:"COOKIE_SECURE" default:"true"`

	// DashboardURL is where OAuth returns the user after login.
	DashboardURL string `yaml:"dashboard_url" env:"DASHBOARD_URL" default:"http://localhost:3000"`

	// StateTTL bounds the OAuth state parameter's lifetime — the CSRF defence
	// for the authorization code flow.
	StateTTL time.Duration `yaml:"state_ttl" env:"STATE_TTL" default:"10m"`
}

// CORS configures cross-origin access for the dashboard.
type CORS struct {
	// AllowedOrigins must be explicit. A wildcard is incompatible with
	// credentialed requests, and our refresh cookie is credentialed.
	AllowedOrigins []string `yaml:"allowed_origins" env:"ALLOWED_ORIGINS" flag:"cors-origins" default:"http://localhost:3000"`
	AllowedMethods []string `yaml:"allowed_methods" env:"ALLOWED_METHODS" default:"GET,POST,PATCH,PUT,DELETE,OPTIONS"`
	AllowedHeaders []string `yaml:"allowed_headers" env:"ALLOWED_HEADERS" default:"Authorization,Content-Type,X-Request-ID"`

	AllowCredentials bool          `yaml:"allow_credentials" env:"ALLOW_CREDENTIALS" default:"true"`
	MaxAge           time.Duration `yaml:"max_age" env:"MAX_AGE" default:"12h"`
}

// RateLimit configures Redis-backed request throttling.
type RateLimit struct {
	Enabled bool `yaml:"enabled" env:"ENABLED" flag:"rate-limit" default:"true"`

	// Requests per Window per identity (user ID when authenticated, else IP).
	Requests int           `yaml:"requests" env:"REQUESTS" default:"120"`
	Window   time.Duration `yaml:"window" env:"WINDOW" default:"1m"`

	// LoginRequests is a tighter budget for the auth endpoints, which are the
	// ones worth brute-forcing.
	LoginRequests int           `yaml:"login_requests" env:"LOGIN_REQUESTS" default:"10"`
	LoginWindow   time.Duration `yaml:"login_window" env:"LOGIN_WINDOW" default:"1m"`
}

// WebSocket configures the realtime gateway.
type WebSocket struct {
	// MaxConnectionsPerUser bounds fan-out per account: several devices plus a
	// few dashboard tabs. Prevents one account from exhausting the gateway.
	MaxConnectionsPerUser int `yaml:"max_connections_per_user" env:"MAX_CONNS_PER_USER" default:"20"`

	ReadBufferSize  int `yaml:"read_buffer_size" env:"READ_BUFFER_SIZE" default:"4096"`
	WriteBufferSize int `yaml:"write_buffer_size" env:"WRITE_BUFFER_SIZE" default:"4096"`

	// SendQueueSize is the per-connection outbound buffer. When it fills, the
	// connection is dropped rather than blocking the broadcaster — one slow
	// consumer must not stall every other client.
	SendQueueSize int `yaml:"send_queue_size" env:"SEND_QUEUE_SIZE" default:"256"`

	// HandshakeTimeout bounds how long an unauthenticated socket may stay open
	// before sending AUTH.
	HandshakeTimeout time.Duration `yaml:"handshake_timeout" env:"HANDSHAKE_TIMEOUT" default:"10s"`
}

// Log configures structured logging.
type Log struct {
	Level     string `yaml:"level" env:"LEVEL" flag:"log-level" default:"info" usage:"debug|info|warn|error"`
	Format    string `yaml:"format" env:"FORMAT" flag:"log-format" default:"json" usage:"json|text"`
	AddSource bool   `yaml:"add_source" env:"ADD_SOURCE" default:"false"`
}

// searchPaths are tried in order when no config file is named explicitly.
// Ordered most-specific first so a repo-local file beats a system-wide one.
var searchPaths = []string{
	"config.yaml",
	"backend/config.yaml",
	"/etc/beuvian/config.yaml",
}

// ErrHelp is returned when the user asked for usage. Re-exported so main can
// treat a help request as a successful exit without importing shared/config.
var ErrHelp = sharedconfig.ErrHelp

// Flags carries operational flags that are not configuration values.
//
// These belong here rather than in main because they are part of the same CLI
// surface: the flag set has to know about them before parsing, or the precedence
// engine rejects them as unknown. Keeping them in one place means `--help` lists
// the complete contract.
type Flags struct {
	// Check validates configuration and exits without starting services.
	Check bool
	// Version prints build information and exits.
	Version bool
	// Migrate applies pending database migrations and exits.
	//
	// A separate mode rather than something the server does at boot: during a
	// rolling deploy several instances start at once, and concurrent DDL can
	// deadlock with the schema half-applied. This runs as one explicit step
	// before new containers are promoted.
	Migrate bool
}

// Load resolves the configuration from all four layers and validates the result.
//
// args should be os.Args[1:]. The returned string names the config file that was
// actually used, or "" if none — reported so startup can log the effective
// source, which is the fastest answer to "why is this setting not what I set?".
//
// Validation is skipped when -check is absent only in the sense that it always
// runs; -check merely stops the caller from proceeding to start services.
func Load(args []string) (*Config, Flags, string, error) {
	cfg := &Config{}
	var ops Flags

	// ContinueOnError so a bad flag returns an error for main to report, rather
	// than the flag package exiting the process from inside a library call.
	fs := flag.NewFlagSet("beuvian-backend", flag.ContinueOnError)
	fs.BoolVar(&ops.Check, "check", false, "validate configuration and exit")
	fs.BoolVar(&ops.Version, "version", false, "print build information and exit")
	fs.BoolVar(&ops.Migrate, "migrate", false, "apply pending database migrations and exit")

	res, err := sharedconfig.Resolve(cfg, sharedconfig.Options{
		Args:        args,
		FlagSet:     fs,
		EnvPrefix:   EnvPrefix,
		Decode:      yaml.Unmarshal,
		ConfigFlag:  "config",
		ConfigEnv:   EnvPrefix + "CONFIG",
		SearchPaths: searchPaths,

		// Railway (and Heroku-style platforms) inject PORT rather than our
		// namespaced variable. Registering it as an alias lets that value take
		// part in the normal precedence chain as an environment-layer input, so
		// an explicit --port flag still wins over it, while BEUVIAN_SERVER_PORT
		// still wins over PORT.
		EnvAliases: map[string]string{
			EnvPrefix + "SERVER_PORT": "PORT",
		},
	})
	if err != nil {
		return nil, ops, res.ConfigFile, err
	}

	// -version must work on a machine with no valid configuration at all, so it
	// short-circuits before validation.
	if ops.Version {
		return cfg, ops, res.ConfigFile, nil
	}

	if err := cfg.Validate(); err != nil {
		return nil, ops, res.ConfigFile, err
	}
	return cfg, ops, res.ConfigFile, nil
}

// Describe returns the effective configuration with secrets redacted, for a
// single startup log line.
func (c *Config) Describe() ([]string, error) { return sharedconfig.Describe(c) }

// normalizeOrigin trims a trailing slash so "https://app.example.com/" and
// "https://app.example.com" compare equal. Browsers send the latter, and the
// mismatch is a genuinely confusing CORS failure to debug.
func normalizeOrigin(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
