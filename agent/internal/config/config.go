// Package config declares and loads the Desktop Agent's configuration.
//
// Uses the same precedence engine as the backend (shared/config), so both
// binaries honour CLI > Env > File > Defaults identically. The agent's defaults
// differ in one important respect: it runs unattended on a user's own machine, so
// paths default into per-user OS-appropriate locations rather than the working
// directory, and a missing config file is normal rather than exceptional.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	sharedconfig "github.com/bhuvan0808/beuviancode/shared/config"
	"gopkg.in/yaml.v3"
)

// EnvPrefix namespaces the agent's environment variables. Distinct from the
// backend's so a developer running both on one machine cannot cross-configure
// them by accident.
const EnvPrefix = "BEUVIAN_AGENT_"

// Config is the Desktop Agent's complete configuration.
type Config struct {
	Backend Backend `yaml:"backend" env:"BACKEND"`
	Device  Device  `yaml:"device" env:"DEVICE"`
	Coding  Coding  `yaml:"coding" env:"CODING"`
	Session Session `yaml:"session" env:"SESSION"`
	Power   Power   `yaml:"power" env:"POWER"`
	Queue   Queue   `yaml:"queue" env:"QUEUE"`
	Log     Log     `yaml:"log" env:"LOG"`
}

// Backend configures the connection to the Beuvian backend.
type Backend struct {
	// URL is the WebSocket gateway endpoint. Defaults to the local backend so a
	// developer running both halves needs no configuration at all.
	URL string `yaml:"url" env:"URL" flag:"backend" default:"ws://localhost:8080/v1/ws" usage:"backend WebSocket URL (wss:// in production)"`

	// APIURL is the REST base for registration and token refresh, which cannot
	// happen over the WebSocket because the socket requires a token to open.
	APIURL string `yaml:"api_url" env:"API_URL" flag:"api-url" default:"http://localhost:8080" usage:"backend REST base URL"`

	// ConnectTimeout bounds a single connection attempt. Short, because the
	// reconnect loop retries; a long timeout just delays the first retry.
	ConnectTimeout time.Duration `yaml:"connect_timeout" env:"CONNECT_TIMEOUT" default:"10s"`

	// InsecureSkipVerify disables TLS verification. Present strictly for testing
	// against a local backend with a self-signed certificate; validation refuses
	// it against a wss:// URL, because that combination is indistinguishable from
	// a machine-in-the-middle attack on a real deployment.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" env:"INSECURE_SKIP_VERIFY" default:"false"`
}

// Device identifies this installation to the backend.
type Device struct {
	// ID is generated on first run and persisted. Empty here means "not yet
	// registered", which is the normal state of a fresh install.
	ID string `yaml:"id" env:"ID"`

	// Name is what the dashboard shows. Defaults to the hostname in Load, since
	// "BODDU-DESKTOP" is more useful to a user than "device 1".
	Name string `yaml:"name" env:"NAME" flag:"device-name" usage:"display name shown in the dashboard"`

	// Token is the device access token. Never written to the plaintext config
	// file: it lives in the encrypted state file (StatePath) instead, which is
	// why it carries secret:"true" and no default.
	Token string `yaml:"-" env:"TOKEN" secret:"true"`

	// StatePath holds the encrypted local state: device ID, tokens, and the
	// offline prompt queue. Defaults to an OS-appropriate per-user directory.
	StatePath string `yaml:"state_path" env:"STATE_PATH" flag:"state" usage:"path to the encrypted agent state file"`
}

// Coding selects and configures the AI coding agent to supervise.
type Coding struct {
	// Adapter names the coding agent. Only "claude" is implemented; the others
	// are registered placeholders that fail loudly if selected.
	Adapter string `yaml:"adapter" env:"ADAPTER" flag:"adapter" default:"claude" usage:"coding agent: claude (codex|gemini|aider|openhands are placeholders)"`

	// ExecutablePath overrides detection. Empty means search PATH.
	ExecutablePath string `yaml:"executable_path" env:"EXECUTABLE_PATH" flag:"exec" usage:"path to the coding agent binary (default: detect on PATH)"`

	// WorkingDirectory is the repository the coding agent operates in.
	//
	// No default on purpose. Defaulting to the current directory would let a
	// misconfigured agent run against the wrong repository, and the coding agent
	// writes files — the blast radius of guessing wrong is real work destroyed.
	WorkingDirectory string `yaml:"working_directory" env:"WORKING_DIRECTORY" flag:"dir" usage:"repository directory the coding agent runs in (required to start a session)"`

	// Args are extra arguments forwarded verbatim to the coding agent.
	Args []string `yaml:"args" env:"ARGS"`

	// AutoStart launches the coding agent as soon as the agent connects. Off by
	// default: starting a coding agent unprompted on a user's machine is a
	// surprising side effect of merely running Beuvian.
	AutoStart bool `yaml:"auto_start" env:"AUTO_START" flag:"auto-start" default:"false"`
}

// Session configures monitoring of the supervised process.
type Session struct {
	// LogBufferLines is the ring buffer of recent output kept in memory for
	// replay when a dashboard connects mid-session.
	LogBufferLines int `yaml:"log_buffer_lines" env:"LOG_BUFFER_LINES" default:"2000"`

	// LogFlushInterval batches output before sending. Without batching, a
	// verbose build emits thousands of frames per second and saturates both the
	// socket and the database.
	LogFlushInterval time.Duration `yaml:"log_flush_interval" env:"LOG_FLUSH_INTERVAL" default:"250ms"`

	// LogBatchSize forces an early flush when a batch fills, so a burst is not
	// held back waiting for the interval.
	LogBatchSize int `yaml:"log_batch_size" env:"LOG_BATCH_SIZE" default:"100"`

	// MaxLogLineBytes truncates pathological single lines (minified bundles,
	// base64 blobs) that would otherwise breach the protocol frame limit.
	MaxLogLineBytes int `yaml:"max_log_line_bytes" env:"MAX_LOG_LINE_BYTES" default:"8192"`

	// IdleTimeout is how long output must be silent before the agent infers the
	// coding agent is waiting for input.
	//
	// This is a heuristic and the value is a genuine tradeoff: too short sends
	// false "waiting for you" notifications while the agent is merely thinking;
	// too long delays the notification the product exists to deliver. 20s is
	// tuned for Claude Code, whose tool calls rarely pause longer.
	IdleTimeout time.Duration `yaml:"idle_timeout" env:"IDLE_TIMEOUT" default:"20s"`

	// StatusInterval is how often a STATUS frame is sent while running, in
	// addition to on every state transition.
	StatusInterval time.Duration `yaml:"status_interval" env:"STATUS_INTERVAL" default:"10s"`
}

// Power configures sleep inhibition.
type Power struct {
	// Enabled allows the agent to prevent sleep during an active session. On by
	// default: a machine that sleeps mid-task defeats the entire product.
	Enabled bool `yaml:"enabled" env:"ENABLED" flag:"prevent-sleep" default:"true"`
}

// Queue configures the offline prompt queue.
type Queue struct {
	// MaxOfflinePrompts caps prompts buffered while disconnected. Bounded so a
	// long outage cannot grow the state file without limit.
	MaxOfflinePrompts int `yaml:"max_offline_prompts" env:"MAX_OFFLINE_PROMPTS" default:"100"`

	// MaxOutboundEvents caps buffered outbound status and log frames. On
	// overflow the oldest are dropped and the batch is marked truncated, which
	// keeps the transcript honest instead of silently lossy.
	MaxOutboundEvents int `yaml:"max_outbound_events" env:"MAX_OUTBOUND_EVENTS" default:"5000"`
}

// Log configures structured logging.
type Log struct {
	Level     string `yaml:"level" env:"LEVEL" flag:"log-level" default:"info" usage:"debug|info|warn|error"`
	Format    string `yaml:"format" env:"FORMAT" flag:"log-format" default:"text" usage:"json|text"`
	AddSource bool   `yaml:"add_source" env:"ADD_SOURCE" default:"false"`

	// FilePath additionally writes logs to a file. Important for a desktop app:
	// when a user reports a problem, stdout is long gone.
	FilePath string `yaml:"file_path" env:"FILE_PATH" flag:"log-file" usage:"also write logs to this file"`
}

// ErrHelp is returned when the user asked for usage, so main can exit 0.
var ErrHelp = sharedconfig.ErrHelp

// Flags carries operational flags that are not configuration values.
type Flags struct {
	// Check validates configuration and exits.
	Check bool
	// Version prints build information and exits.
	Version bool
	// Detect probes for installed coding agents and exits. Useful as the first
	// diagnostic when a user reports "Beuvian cannot find Claude".
	Detect bool
}

// Load resolves the agent's configuration and validates it.
func Load(args []string) (*Config, Flags, string, error) {
	cfg := &Config{}
	var ops Flags

	fs := flag.NewFlagSet("beuvian-agent", flag.ContinueOnError)
	fs.BoolVar(&ops.Check, "check", false, "validate configuration and exit")
	fs.BoolVar(&ops.Version, "version", false, "print build information and exit")
	fs.BoolVar(&ops.Detect, "detect", false, "list installed coding agents and exit")

	res, err := sharedconfig.Resolve(cfg, sharedconfig.Options{
		Args:        args,
		FlagSet:     fs,
		EnvPrefix:   EnvPrefix,
		Decode:      yaml.Unmarshal,
		ConfigFlag:  "config",
		ConfigEnv:   EnvPrefix + "CONFIG",
		SearchPaths: SearchPaths(),
	})
	if err != nil {
		return nil, ops, res.ConfigFile, err
	}

	// Defaults that cannot be expressed as a static tag because they depend on
	// the host. Applied only when no layer supplied a value, so they sit at the
	// bottom of the precedence chain where a default belongs.
	if cfg.Device.Name == "" {
		if host, herr := os.Hostname(); herr == nil {
			cfg.Device.Name = host
		} else {
			cfg.Device.Name = "unknown-device"
		}
	}
	if cfg.Device.StatePath == "" {
		cfg.Device.StatePath = DefaultStatePath()
	}

	if ops.Version {
		return cfg, ops, res.ConfigFile, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, ops, res.ConfigFile, err
	}
	return cfg, ops, res.ConfigFile, nil
}

// Describe returns the effective configuration with secrets redacted.
func (c *Config) Describe() ([]string, error) { return sharedconfig.Describe(c) }

// configDir returns the per-user directory for Beuvian's files.
//
// Follows each platform's convention rather than imposing one: users and their
// backup tools expect application data in the platform's usual place, and a
// dotfile in $HOME on Windows is wrong.
func configDir() string {
	switch runtime.GOOS {
	case "windows":
		// %AppData%\Beuvian — roams with the user profile.
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "Beuvian")
		}
	case "darwin":
		// ~/Library/Application Support/Beuvian
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "Beuvian")
		}
	default:
		// ~/.config/beuvian, honouring XDG_CONFIG_HOME via UserConfigDir.
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "beuvian")
		}
	}
	// Last resort: the working directory. Not ideal, but a running agent beats
	// one that refuses to start because it could not resolve a home directory.
	return "."
}

// SearchPaths lists config file locations in precedence order: the working
// directory first so a developer's local file wins over their installed one.
func SearchPaths() []string {
	return []string{
		"beuvian-agent.yaml",
		"config.yaml",
		filepath.Join(configDir(), "config.yaml"),
	}
}

// DefaultStatePath is where encrypted device state is stored.
func DefaultStatePath() string {
	return filepath.Join(configDir(), "agent.state")
}

// String renders the resolved backend endpoints, for startup logging.
func (b Backend) String() string {
	return fmt.Sprintf("ws=%s api=%s", b.URL, b.APIURL)
}
