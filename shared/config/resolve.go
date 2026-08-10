package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
)

// Options configures Resolve.
type Options struct {
	// Args is the command line without argv[0]. Usually os.Args[1:].
	Args []string

	// FlagSet receives the generated flags. If nil, one is created with
	// ContinueOnError so a bad flag returns an error instead of exiting the
	// process — a library must not call os.Exit on its caller's behalf.
	FlagSet *flag.FlagSet

	// EnvPrefix is prepended to every env name, e.g. "BEUVIAN_".
	EnvPrefix string

	// Decode parses the config file. Pass yaml.Unmarshal. If nil, the file
	// layer is skipped entirely.
	Decode DecodeFunc

	// ConfigFlag is the flag naming the config file. Defaults to "config".
	ConfigFlag string

	// ConfigEnv is the env var naming the config file, e.g. "BEUVIAN_CONFIG".
	ConfigEnv string

	// SearchPaths are tried in order when neither the flag nor the env var
	// names a file. A missing search path is not an error; a missing
	// explicitly-requested file is.
	SearchPaths []string

	// EnvAliases maps a fully-prefixed variable name to a platform-conventional
	// fallback, consulted only when the primary is unset:
	//
	//	{"BEUVIAN_SERVER_PORT": "PORT"}
	//
	// Railway and similar platforms inject PORT rather than our namespaced name.
	// Registering the alias lets that value take part in the normal precedence
	// chain — so an explicit --port flag still wins — without copying it into our
	// namespace via os.Setenv, which would mutate the process environment as a
	// side effect of loading configuration.
	EnvAliases map[string]string
}

// Result reports what Resolve actually did.
type Result struct {
	// ConfigFile is the file that was loaded, or "" if none was.
	//
	// Returned rather than merely logged so the caller can report the effective
	// source at startup. "Why is this setting wrong?" is nearly always answered
	// by "a different file than you think was loaded".
	ConfigFile string

	// FlagSet is the set used, for printing usage.
	FlagSet *flag.FlagSet
}

// ErrHelp is returned when the user asked for usage. Callers should print usage
// and exit 0 — a help request is a success, not a failure.
var ErrHelp = flag.ErrHelp

// Resolve populates dst from all four layers in PROJECT.md's mandated order:
// defaults, then the config file, then the environment, then explicit CLI flags.
//
// Two passes over the command line are unavoidable, and the reason is worth
// stating: the location of the config file is itself configurable via flag and
// env, so flags must be parsed *before* the file can be read, yet applied *after*
// it so they retain top priority. Pass one parses and records; pass two replays
// only the flags the user actually typed.
func Resolve(dst any, o Options) (Result, error) {
	loader, err := New(dst)
	if err != nil {
		return Result{}, err
	}

	fs := o.FlagSet
	if fs == nil {
		fs = flag.NewFlagSet("beuvian", flag.ContinueOnError)
	}
	configFlag := o.ConfigFlag
	if configFlag == "" {
		configFlag = "config"
	}

	// Register the config-path flag unless the target struct already claims it.
	var configPathFlag *string
	if fs.Lookup(configFlag) == nil {
		configPathFlag = fs.String(configFlag, "",
			"path to config.yaml (overrides "+orNone(o.ConfigEnv)+" and the default search paths)")
	}

	loader.BindFlags(fs)

	// Pass one: parse. ContinueOnError means a usage request surfaces as ErrHelp
	// rather than terminating the process.
	if err := fs.Parse(o.Args); err != nil {
		return Result{FlagSet: fs}, err
	}

	// Resolve which file to read, honouring the same precedence: flag, env, then
	// the search paths.
	var path string
	explicit := false
	if configPathFlag != nil && *configPathFlag != "" {
		path, explicit = *configPathFlag, true
	} else if o.ConfigEnv != "" {
		if v, ok := os.LookupEnv(o.ConfigEnv); ok && v != "" {
			path, explicit = v, true
		}
	}
	if path == "" {
		for _, candidate := range o.SearchPaths {
			if fileExists(candidate) {
				path = candidate
				break
			}
		}
	}

	// Layer one: defaults.
	if err := loader.ApplyDefaults(); err != nil {
		return Result{FlagSet: fs}, err
	}

	// Layer two: the config file.
	res := Result{FlagSet: fs}
	if path != "" && o.Decode != nil {
		// The path originates from the caller's own CLI/env/config chain —
		// loading an arbitrary user-chosen file is precisely the resolver's
		// contract, so this is the feature rather than an injection vector.
		data, readErr := os.ReadFile(path) //nolint:gosec // G304: user-chosen config path by design
		switch {
		case readErr == nil:
			if err := loader.ApplyFile(data, o.Decode, dst); err != nil {
				return res, fmt.Errorf("config %s: %w", path, err)
			}
			res.ConfigFile = path
		case explicit:
			// The user named this file. Silently ignoring it would run with
			// settings they did not ask for — fail loudly instead.
			return res, fmt.Errorf("config: cannot read %s: %w", path, readErr)
		case !errors.Is(readErr, os.ErrNotExist):
			// A search-path candidate that exists but is unreadable (bad
			// permissions) is also worth surfacing.
			return res, fmt.Errorf("config: cannot read %s: %w", path, readErr)
		}
	}

	// Layer three: the environment.
	if err := loader.ApplyEnv(o.EnvPrefix, o.EnvAliases); err != nil {
		return res, err
	}

	// Layer four: explicit CLI flags, highest priority.
	if err := loader.ApplyFlags(); err != nil {
		return res, err
	}

	return res, nil
}

// Describe returns the effective configuration as sorted "path=value" lines,
// with secret-tagged fields redacted.
//
// Intended for a single startup log line. Logging the effective configuration is
// how a misconfigured deploy gets diagnosed in one look instead of by bisecting
// environment variables; redaction is what makes doing so safe.
func Describe(dst any) ([]string, error) {
	loader, err := New(dst)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(loader.fields))
	for _, f := range loader.fields {
		val := "<redacted>"
		if !f.secret {
			val = fmt.Sprintf("%v", f.value.Interface())
		} else if isZero(f.value) {
			// Distinguish "secret not set" from "secret set" — the former is
			// usually the actual bug.
			val = "<unset>"
		}
		out = append(out, f.path+"="+val)
	}
	sort.Strings(out)
	return out, nil
}

// Usage renders the flag usage text for a target struct, for --help output.
func Usage(dst any, name string) (string, error) {
	loader, err := New(dst)
	if err != nil {
		return "", err
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var sb strings.Builder
	fs.SetOutput(&sb)
	loader.BindFlags(fs)
	fs.PrintDefaults()
	return sb.String(), nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func isZero(v reflect.Value) bool { return v.IsZero() }

func orNone(s string) string {
	if s == "" {
		return "(no env var)"
	}
	return s
}
