package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhuvan0808/beuviancode/agent/internal/config"
)

func load(t *testing.T, args ...string) *config.Config {
	t.Helper()
	cfg, _, _, err := config.Load(args)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestFreshInstallNeedsNoConfiguration(t *testing.T) {
	// A user downloads a binary and runs it. That must work.
	cfg, ops, file, err := config.Load(nil)
	if err != nil {
		t.Fatalf("a fresh install must produce a valid configuration: %v", err)
	}
	if file != "" {
		t.Errorf("no config file expected, got %q", file)
	}
	if ops.Check || ops.Version || ops.Detect {
		t.Error("operational flags should default to false")
	}
	if cfg.Coding.Adapter != "claude" {
		t.Errorf("Adapter = %q, want claude", cfg.Coding.Adapter)
	}
	// Host-dependent defaults cannot be static tags, so Load fills them in.
	if cfg.Device.Name == "" {
		t.Error("Device.Name should default to the hostname")
	}
	if cfg.Device.StatePath == "" {
		t.Error("Device.StatePath should default to an OS-appropriate path")
	}
	// The working directory must NOT be defaulted: the coding agent writes files,
	// so guessing wrong destroys real work.
	if cfg.Coding.WorkingDirectory != "" {
		t.Errorf("WorkingDirectory should have no default, got %q", cfg.Coding.WorkingDirectory)
	}
}

func TestStatePathIsPerUserNotCwd(t *testing.T) {
	// The state file holds a device token. Writing it into whatever directory the
	// agent happened to start in would scatter credentials across a user's disk.
	cfg := load(t)
	if !filepath.IsAbs(cfg.Device.StatePath) {
		t.Errorf("StatePath = %q, want an absolute per-user path", cfg.Device.StatePath)
	}
}

func TestBackendURLSchemeIsEnforced(t *testing.T) {
	tests := []struct {
		name, url string
		valid     bool
	}{
		{"ws", "ws://localhost:8080/v1/ws", true},
		{"wss", "wss://api.example.com/v1/ws", true},
		{"http is not a websocket scheme", "http://localhost:8080/v1/ws", false},
		{"https is not a websocket scheme", "https://api.example.com/v1/ws", false},
		{"missing scheme", "localhost:8080/v1/ws", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := load(t)
			cfg.Backend.URL = tc.url
			err := cfg.Validate()
			if tc.valid && err != nil {
				t.Errorf("%q should be valid: %v", tc.url, err)
			}
			if !tc.valid && err == nil {
				t.Errorf("%q should be rejected", tc.url)
			}
		})
	}
}

func TestInsecureSkipVerifyIsRefusedForTLSEndpoints(t *testing.T) {
	// Disabling verification against a real wss:// endpoint is indistinguishable
	// from accepting a machine-in-the-middle. The escape hatch is for local
	// self-signed testing over ws:// only.
	cfg := load(t)
	cfg.Backend.URL = "wss://api.example.com/v1/ws"
	cfg.Backend.InsecureSkipVerify = true

	err := cfg.Validate()
	if err == nil {
		t.Fatal("insecure_skip_verify must be refused for a wss:// endpoint")
	}
	if !strings.Contains(err.Error(), "insecure_skip_verify") {
		t.Errorf("error should name the setting, got: %v", err)
	}

	// Permitted over plaintext ws://, where there is no TLS to undermine.
	cfg = load(t)
	cfg.Backend.URL = "ws://localhost:8080/v1/ws"
	cfg.Backend.InsecureSkipVerify = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("insecure_skip_verify over ws:// should be permitted: %v", err)
	}
}

func TestWorkingDirectoryMustExistWhenSet(t *testing.T) {
	cfg := load(t)
	cfg.Coding.WorkingDirectory = filepath.Join(t.TempDir(), "does-not-exist")

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a configured working directory that does not exist must be rejected")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should say the directory is missing, got: %v", err)
	}

	// An existing directory is fine, and so is an empty one (chosen later from the
	// dashboard).
	cfg = load(t)
	cfg.Coding.WorkingDirectory = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Errorf("an existing directory should be accepted: %v", err)
	}
}

func TestWorkingDirectoryRejectsAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir.txt")
	if err := writeEmpty(file); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cfg := load(t)
	cfg.Coding.WorkingDirectory = file
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a file must not be accepted as a working directory")
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("error should say it is not a directory, got: %v", err)
	}
}

func TestAutoStartRequiresAWorkingDirectory(t *testing.T) {
	// The contradiction would otherwise fail at launch, after the user believed
	// startup had succeeded.
	cfg := load(t)
	cfg.Coding.AutoStart = true
	cfg.Coding.WorkingDirectory = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("auto_start with no working directory must be rejected")
	}
	if !strings.Contains(err.Error(), "auto_start requires") {
		t.Errorf("error should explain the dependency, got: %v", err)
	}
}

func TestDeviceIDShapeIsChecked(t *testing.T) {
	// A typo'd ID should fail here rather than as an authentication failure.
	cfg := load(t)
	cfg.Device.ID = "device-1"
	if err := cfg.Validate(); err == nil {
		t.Error("an ID without the dev_ prefix should be rejected")
	}

	cfg = load(t)
	cfg.Device.ID = "dev_01J9Z3K7QF8XKM2N4P6R8T0VWY"
	if err := cfg.Validate(); err != nil {
		t.Errorf("a well-formed device ID should be accepted: %v", err)
	}
}

func TestLogBatchCannotExceedTheRingBuffer(t *testing.T) {
	// A batch larger than the buffer could never be filled from it, so the
	// early-flush path would be dead and every flush would wait for the timer.
	cfg := load(t)
	cfg.Session.LogBufferLines = 50
	cfg.Session.LogBatchSize = 100

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a batch size larger than the buffer must be rejected")
	}
	if !strings.Contains(err.Error(), "cannot exceed log_buffer_lines") {
		t.Errorf("error should explain the relationship, got: %v", err)
	}
}

func TestOfflineQueueMustHoldAtLeastOnePrompt(t *testing.T) {
	cfg := load(t)
	cfg.Queue.MaxOfflinePrompts = 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a zero-length offline queue must be rejected")
	}
	// The message should say what the consequence is, not just that it is invalid.
	if !strings.Contains(err.Error(), "prompts sent while offline are lost") {
		t.Errorf("error should explain the consequence, got: %v", err)
	}
}

func TestPrecedenceCLIBeatsEnvironment(t *testing.T) {
	t.Setenv("BEUVIAN_AGENT_CODING_ADAPTER", "aider")
	if cfg := load(t); cfg.Coding.Adapter != "aider" {
		t.Errorf("Adapter = %q, want the environment value", cfg.Coding.Adapter)
	}
	if cfg := load(t, "-adapter", "claude"); cfg.Coding.Adapter != "claude" {
		t.Errorf("Adapter = %q, want the CLI flag to win", cfg.Coding.Adapter)
	}
}

func TestAgentUsesItsOwnEnvPrefix(t *testing.T) {
	// A developer running both binaries on one machine must not be able to
	// cross-configure them.
	t.Setenv("BEUVIAN_LOG_LEVEL", "error") // the BACKEND's variable
	if cfg := load(t); cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q; the backend's variable must not affect the agent", cfg.Log.Level)
	}

	t.Setenv("BEUVIAN_AGENT_LOG_LEVEL", "warn")
	if cfg := load(t); cfg.Log.Level != "warn" {
		t.Errorf("Log.Level = %q, want warn from the agent's own variable", cfg.Log.Level)
	}
}

func TestTokenIsNeverReadFromTheConfigFile(t *testing.T) {
	// Device.Token carries yaml:"-" so it cannot be loaded from the plaintext
	// config file; it belongs in the encrypted state store. Environment remains
	// available for testing.
	t.Setenv("BEUVIAN_AGENT_DEVICE_TOKEN", "tok_from_env")
	if cfg := load(t); cfg.Device.Token != "tok_from_env" {
		t.Errorf("Token = %q, want the environment value", cfg.Device.Token)
	}
}

func TestDescribeRedactsTheDeviceToken(t *testing.T) {
	t.Setenv("BEUVIAN_AGENT_DEVICE_TOKEN", "super-secret-device-token")
	cfg := load(t)

	lines, err := cfg.Describe()
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "super-secret-device-token") {
		t.Errorf("Describe leaked the device token:\n%s", joined)
	}
	if !strings.Contains(joined, "Device.Token=<redacted>") {
		t.Errorf("expected the token to be redacted:\n%s", joined)
	}
}

func TestSearchPathsPreferTheWorkingDirectory(t *testing.T) {
	paths := config.SearchPaths()
	if len(paths) < 2 {
		t.Fatalf("expected several search paths, got %v", paths)
	}
	// A developer's local file must beat their installed one.
	if paths[0] != "beuvian-agent.yaml" {
		t.Errorf("first search path = %q, want the working-directory file", paths[0])
	}
	if !filepath.IsAbs(paths[len(paths)-1]) {
		t.Errorf("last search path = %q, want the absolute per-user path", paths[len(paths)-1])
	}
}

func TestInvalidLogSettingsAreRejected(t *testing.T) {
	cfg := load(t)
	cfg.Log.Level = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Error("an unknown log level should be rejected")
	}

	cfg = load(t)
	cfg.Log.Format = "xml"
	if err := cfg.Validate(); err == nil {
		t.Error("an unknown log format should be rejected")
	}
}

func writeEmpty(path string) error {
	return os.WriteFile(path, nil, 0o600)
}
