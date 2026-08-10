package config_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/config"
)

// sample mirrors the shape of a real Beuvian config: nested sections, durations,
// slices, and a secret.
type sample struct {
	Env string `json:"env" env:"ENV" flag:"env" default:"development"`

	Server struct {
		Host string        `json:"host" env:"HOST" flag:"host" default:"0.0.0.0"`
		Port int           `json:"port" env:"PORT" flag:"port" default:"8080"`
		Idle time.Duration `json:"idle" env:"IDLE" flag:"idle" default:"30s"`
	} `json:"server" env:"SERVER"`

	Security struct {
		JWTSecret string   `json:"jwt_secret" env:"JWT_SECRET" secret:"true"`
		Origins   []string `json:"origins" env:"ORIGINS" flag:"origins" default:"http://localhost:3000"`
		Debug     bool     `json:"debug" env:"DEBUG" flag:"debug" default:"false"`
	} `json:"security" env:"SECURITY"`
}

// decodeJSON stands in for yaml.Unmarshal. Using encoding/json keeps this test
// inside the standard library, preserving the module's zero-dependency invariant
// while exercising the identical DecodeFunc contract.
func decodeJSON(data []byte, dst any) error { return json.Unmarshal(data, dst) }

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

func TestDefaultsOnly(t *testing.T) {
	var cfg sample
	if _, err := config.Resolve(&cfg, config.Options{Args: nil, EnvPrefix: "TSTA_"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Env != "development" {
		t.Errorf("Env = %q, want development", cfg.Env)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.Idle != 30*time.Second {
		t.Errorf("Idle = %v, want 30s", cfg.Server.Idle)
	}
	if len(cfg.Security.Origins) != 1 || cfg.Security.Origins[0] != "http://localhost:3000" {
		t.Errorf("Origins = %v", cfg.Security.Origins)
	}
	if cfg.Security.Debug {
		t.Error("Debug should default to false")
	}
}

// TestPrecedenceOrder is the load-bearing test for this package: PROJECT.md
// mandates CLI > Env > File > Defaults, and all four layers set the same field
// here so only the correct winner passes.
func TestPrecedenceOrder(t *testing.T) {
	file := writeFile(t, `{"server":{"port":2222},"env":"from-file"}`)

	t.Run("file beats defaults", func(t *testing.T) {
		var cfg sample
		_, err := config.Resolve(&cfg, config.Options{
			Args: nil, EnvPrefix: "TSTB_", Decode: decodeJSON,
			ConfigFlag: "config", SearchPaths: []string{file},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cfg.Server.Port != 2222 {
			t.Errorf("Port = %d, want 2222 from the file", cfg.Server.Port)
		}
	})

	t.Run("env beats file", func(t *testing.T) {
		t.Setenv("TSTC_SERVER_PORT", "3333")
		var cfg sample
		_, err := config.Resolve(&cfg, config.Options{
			Args: nil, EnvPrefix: "TSTC_", Decode: decodeJSON,
			SearchPaths: []string{file},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cfg.Server.Port != 3333 {
			t.Errorf("Port = %d, want 3333 from the environment", cfg.Server.Port)
		}
	})

	t.Run("flag beats env and file", func(t *testing.T) {
		t.Setenv("TSTD_SERVER_PORT", "3333")
		var cfg sample
		_, err := config.Resolve(&cfg, config.Options{
			Args: []string{"-port", "4444"}, EnvPrefix: "TSTD_", Decode: decodeJSON,
			SearchPaths: []string{file},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cfg.Server.Port != 4444 {
			t.Errorf("Port = %d, want 4444 from the CLI", cfg.Server.Port)
		}
	})

	t.Run("all four layers at once", func(t *testing.T) {
		t.Setenv("TSTE_SERVER_PORT", "3333")
		t.Setenv("TSTE_ENV", "from-env")
		var cfg sample
		_, err := config.Resolve(&cfg, config.Options{
			Args: []string{"-port", "4444"}, EnvPrefix: "TSTE_", Decode: decodeJSON,
			SearchPaths: []string{file},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		// Port: set by all four -> CLI wins.
		if cfg.Server.Port != 4444 {
			t.Errorf("Port = %d, want 4444 (CLI)", cfg.Server.Port)
		}
		// Env field: file and env set it, no flag -> env wins.
		if cfg.Env != "from-env" {
			t.Errorf("Env = %q, want from-env", cfg.Env)
		}
		// Host: only a default exists -> default survives.
		if cfg.Server.Host != "0.0.0.0" {
			t.Errorf("Host = %q, want the default", cfg.Server.Host)
		}
	})
}

// TestUnsetFlagDoesNotOverride guards the subtlest bug in the design: if flags
// were applied via VisitAll instead of Visit, every registered-but-untyped flag
// would overwrite the environment and file with its empty default.
func TestUnsetFlagDoesNotOverride(t *testing.T) {
	t.Setenv("TSTF_SERVER_HOST", "10.0.0.5")
	file := writeFile(t, `{"env":"from-file"}`)

	var cfg sample
	_, err := config.Resolve(&cfg, config.Options{
		// A flag IS passed, but not -host or -env.
		Args: []string{"-port", "9999"}, EnvPrefix: "TSTF_", Decode: decodeJSON,
		SearchPaths: []string{file},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Server.Host != "10.0.0.5" {
		t.Errorf("Host = %q, want the env value to survive an unset flag", cfg.Server.Host)
	}
	if cfg.Env != "from-file" {
		t.Errorf("Env = %q, want the file value to survive an unset flag", cfg.Env)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Server.Port)
	}
}

func TestEmptyEnvValueIsAnIntentionalOverride(t *testing.T) {
	// LookupEnv, not Getenv: explicitly exporting an empty value must clear the
	// setting rather than read as "unset".
	t.Setenv("TSTG_ENV", "")
	var cfg sample
	if _, err := config.Resolve(&cfg, config.Options{EnvPrefix: "TSTG_"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Env != "" {
		t.Errorf("Env = %q, want the empty override to win over the default", cfg.Env)
	}
}

func TestNestedEnvPrefixComposition(t *testing.T) {
	t.Setenv("TSTH_SECURITY_JWT_SECRET", "s3cr3t")
	t.Setenv("TSTH_SERVER_IDLE", "90s")
	var cfg sample
	if _, err := config.Resolve(&cfg, config.Options{EnvPrefix: "TSTH_"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Security.JWTSecret != "s3cr3t" {
		t.Errorf("JWTSecret = %q", cfg.Security.JWTSecret)
	}
	if cfg.Server.Idle != 90*time.Second {
		t.Errorf("Idle = %v, want 90s", cfg.Server.Idle)
	}
}

func TestSliceParsing(t *testing.T) {
	t.Setenv("TSTI_SECURITY_ORIGINS", "https://a.com, https://b.com ,,https://c.com")
	var cfg sample
	if _, err := config.Resolve(&cfg, config.Options{EnvPrefix: "TSTI_"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"https://a.com", "https://b.com", "https://c.com"}
	if len(cfg.Security.Origins) != len(want) {
		t.Fatalf("Origins = %v, want %v", cfg.Security.Origins, want)
	}
	for i := range want {
		if cfg.Security.Origins[i] != want[i] {
			t.Errorf("Origins[%d] = %q, want %q", i, cfg.Security.Origins[i], want[i])
		}
	}
}

func TestInvalidValuesReportActionableErrors(t *testing.T) {
	cases := []struct{ name, env, val, wantSubstr string }{
		{"bad int", "TSTJ_SERVER_PORT", "eighty", "invalid integer"},
		{"bad duration", "TSTJ_SERVER_IDLE", "30 seconds", "invalid duration"},
		{"bad bool", "TSTJ_SECURITY_DEBUG", "yes-please", "invalid boolean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			var cfg sample
			_, err := config.Resolve(&cfg, config.Options{EnvPrefix: "TSTJ_"})
			if err == nil {
				t.Fatal("expected an error for a malformed value")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q should mention %q", err, tc.wantSubstr)
			}
			// The message must name the offending variable so it is actionable.
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q should name the variable %q", err, tc.env)
			}
		})
	}
}

func TestExplicitlyRequestedFileMustExist(t *testing.T) {
	var cfg sample
	_, err := config.Resolve(&cfg, config.Options{
		Args:   []string{"-config", filepath.Join(t.TempDir(), "absent.json")},
		Decode: decodeJSON, EnvPrefix: "TSTK_",
	})
	if err == nil {
		t.Fatal("a config file named explicitly on the CLI must fail loudly when missing")
	}
}

func TestMissingSearchPathIsNotAnError(t *testing.T) {
	var cfg sample
	res, err := config.Resolve(&cfg, config.Options{
		Decode: decodeJSON, EnvPrefix: "TSTL_",
		SearchPaths: []string{filepath.Join(t.TempDir(), "absent.json")},
	})
	if err != nil {
		t.Fatalf("an absent search-path candidate should be skipped silently: %v", err)
	}
	if res.ConfigFile != "" {
		t.Errorf("ConfigFile = %q, want empty", res.ConfigFile)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("defaults should still apply, got Port = %d", cfg.Server.Port)
	}
}

func TestResultReportsLoadedFile(t *testing.T) {
	file := writeFile(t, `{"env":"staging"}`)
	var cfg sample
	res, err := config.Resolve(&cfg, config.Options{
		Decode: decodeJSON, EnvPrefix: "TSTM_", SearchPaths: []string{file},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.ConfigFile != file {
		t.Errorf("ConfigFile = %q, want %q", res.ConfigFile, file)
	}
}

func TestConfigPathFromEnv(t *testing.T) {
	file := writeFile(t, `{"env":"from-env-path"}`)
	t.Setenv("TSTN_CONFIG", file)
	var cfg sample
	res, err := config.Resolve(&cfg, config.Options{
		Decode: decodeJSON, EnvPrefix: "TSTN_", ConfigEnv: "TSTN_CONFIG",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.ConfigFile != file || cfg.Env != "from-env-path" {
		t.Errorf("config path from env not honoured: file=%q env=%q", res.ConfigFile, cfg.Env)
	}
}

func TestEnvAliasIsConsultedOnlyWhenPrimaryIsUnset(t *testing.T) {
	// Hosting platforms inject their own conventional names (Railway sets PORT).
	// The alias must participate in the environment layer without displacing our
	// own variable and without mutating the process environment.
	aliases := map[string]string{"TSTP_SERVER_PORT": "PORT"}

	t.Run("alias applies when primary is unset", func(t *testing.T) {
		t.Setenv("PORT", "5555")
		var cfg sample
		if _, err := config.Resolve(&cfg, config.Options{EnvPrefix: "TSTP_", EnvAliases: aliases}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cfg.Server.Port != 5555 {
			t.Errorf("Port = %d, want 5555 from the alias", cfg.Server.Port)
		}
	})

	t.Run("primary beats alias", func(t *testing.T) {
		t.Setenv("PORT", "5555")
		t.Setenv("TSTP_SERVER_PORT", "6666")
		var cfg sample
		if _, err := config.Resolve(&cfg, config.Options{EnvPrefix: "TSTP_", EnvAliases: aliases}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cfg.Server.Port != 6666 {
			t.Errorf("Port = %d, want our own variable to win", cfg.Server.Port)
		}
	})

	t.Run("explicit flag beats alias", func(t *testing.T) {
		t.Setenv("PORT", "5555")
		var cfg sample
		_, err := config.Resolve(&cfg, config.Options{
			Args: []string{"-port", "7777"}, EnvPrefix: "TSTP_", EnvAliases: aliases,
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cfg.Server.Port != 7777 {
			t.Errorf("Port = %d, want the CLI flag to win", cfg.Server.Port)
		}
	})

	t.Run("resolution does not mutate the environment", func(t *testing.T) {
		// The bug this guards: copying PORT into our namespace with os.Setenv makes
		// a second Resolve behave differently from the first and leaks across tests.
		t.Setenv("PORT", "5555")
		var cfg sample
		if _, err := config.Resolve(&cfg, config.Options{EnvPrefix: "TSTP_", EnvAliases: aliases}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if v, ok := os.LookupEnv("TSTP_SERVER_PORT"); ok {
			t.Errorf("Resolve set TSTP_SERVER_PORT=%q; it must not mutate the environment", v)
		}
	})
}

func TestDescribeRedactsSecrets(t *testing.T) {
	var cfg sample
	if _, err := config.Resolve(&cfg, config.Options{EnvPrefix: "TSTO_"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cfg.Security.JWTSecret = "super-secret-signing-key"

	lines, err := config.Describe(&cfg)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "super-secret-signing-key") {
		t.Errorf("Describe leaked a secret:\n%s", joined)
	}
	if !strings.Contains(joined, "Security.JWTSecret=<redacted>") {
		t.Errorf("expected the secret field to be redacted, got:\n%s", joined)
	}
	// Non-secret values must still be visible, or the dump is useless.
	if !strings.Contains(joined, "Server.Port=8080") {
		t.Errorf("expected non-secret values to be shown, got:\n%s", joined)
	}
}

func TestDescribeDistinguishesUnsetSecret(t *testing.T) {
	var cfg sample
	lines, err := config.Describe(&cfg)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	// "secret is missing" is usually the actual production bug, so it must be
	// distinguishable from "secret is present".
	if !strings.Contains(strings.Join(lines, "\n"), "Security.JWTSecret=<unset>") {
		t.Errorf("an empty secret should render as <unset>, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestInvalidTargets(t *testing.T) {
	var notAStruct int
	for _, bad := range []any{nil, notAStruct, &notAStruct, sample{}} {
		if _, err := config.New(bad); err == nil {
			t.Errorf("New(%T) should fail; only non-nil struct pointers are valid", bad)
		}
	}
}

func TestHelpRequestIsDistinguishable(t *testing.T) {
	var cfg sample
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{}) // suppress usage output during tests
	_, err := config.Resolve(&cfg, config.Options{Args: []string{"-h"}, FlagSet: fs})
	if err != config.ErrHelp {
		t.Errorf("err = %v, want ErrHelp so callers can exit 0 for --help", err)
	}
}

func TestUsageListsFlags(t *testing.T) {
	var cfg sample
	out, err := config.Usage(&cfg, "beuvian-test")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for _, want := range []string{"-port", "-host", "-env", "-origins"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q:\n%s", want, out)
		}
	}
	// Defaults belong in help text; a user should not have to read source to
	// learn what a flag does when omitted.
	if !strings.Contains(out, "8080") {
		t.Errorf("usage output should mention default values:\n%s", out)
	}
}
