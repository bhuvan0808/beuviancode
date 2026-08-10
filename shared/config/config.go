// Package config implements Beuvian's configuration precedence engine.
//
// PROJECT.md mandates the resolution order CLI > Environment > Config file >
// Defaults, for both the backend and the Desktop Agent. Two independent loaders
// would be duplicated logic that drifts, so the precedence engine lives here,
// once.
//
// The awkward part is that parsing config.yaml requires a YAML library, and this
// module is contractually dependency-free (see the module comment in go.mod). The
// resolution: this package never parses a file. It takes a DecodeFunc — the
// caller supplies gopkg.in/yaml.v3's Unmarshal — so the *policy* (what beats what)
// is centralised while the *format* dependency stays in the binaries that need it.
// That also means a future binary reading TOML or JSON reuses this untouched.
//
// Layer responsibilities:
//   - Defaults and env and flags are applied here, driven by struct tags.
//   - The file layer is delegated to DecodeFunc, which honours the struct's own
//     `yaml:"..."` tags. A decoder only writes keys present in the document, so
//     absent keys correctly leave the defaults standing.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Struct tags recognised by the engine.
//
//	env:"PORT"          environment variable name, joined to the prefix
//	flag:"port"         CLI flag name
//	default:"8080"      value used when no layer supplies one
//	usage:"HTTP port"   flag help text
//	secret:"true"       redact this value from String() output
//
// A field with no env/flag tag is still reachable through the config file, which
// is intentional: rarely-tuned settings should not clutter `--help`.
const (
	tagEnv     = "env"
	tagFlag    = "flag"
	tagDefault = "default"
	tagUsage   = "usage"
	tagSecret  = "secret"
)

// DecodeFunc unmarshals a serialised config document into dst.
//
// Matches the signature of yaml.Unmarshal and json.Unmarshal, so callers pass
// the library function directly with no adapter.
type DecodeFunc func(data []byte, dst any) error

// ErrInvalidTarget is returned when the destination is not a non-nil struct
// pointer.
var ErrInvalidTarget = errors.New("config: target must be a non-nil pointer to a struct")

// field is one settable leaf discovered by reflection.
type field struct {
	path    string // dotted path, for error messages: "server.port"
	value   reflect.Value
	envName string
	flagRef string
	defVal  string
	usage   string
	secret  bool
}

// durationType lets walk treat time.Duration as a leaf rather than as an int64.
var durationType = reflect.TypeOf(time.Duration(0))

// Loader applies configuration layers to a target struct.
//
// Stateful by necessity: it must remember which flags the user actually typed so
// the CLI layer can be re-applied last, after the file and environment layers
// have run. See Resolve for why that ordering requires two passes.
type Loader struct {
	fields []*field

	// flagValues holds the raw string captured for each registered flag.
	flagValues map[string]*string
	// flagSet is retained so ApplyFlags can consult Visit for explicit sets.
	flagSet *flag.FlagSet
}

// New reflects over dst and prepares a Loader.
func New(dst any) (*Loader, error) {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil, ErrInvalidTarget
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return nil, ErrInvalidTarget
	}
	l := &Loader{flagValues: map[string]*string{}}
	l.walk(elem, "", "")
	return l, nil
}

// walk collects settable leaves, recursing into nested structs.
//
// envPrefix accumulates across nesting so a nested struct can declare short env
// names without repeating its parent's prefix.
func (l *Loader) walk(v reflect.Value, pathPrefix, envPrefix string) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		fv := v.Field(i)

		// Unexported fields are unsettable; skip rather than panic.
		if !fv.CanSet() {
			continue
		}

		name := sf.Name
		path := name
		if pathPrefix != "" {
			path = pathPrefix + "." + name
		}

		env := sf.Tag.Get(tagEnv)
		fullEnv := env
		if env != "" && envPrefix != "" {
			fullEnv = envPrefix + env
		}

		// Recurse into plain structs (but not time.Duration or time.Time).
		if fv.Kind() == reflect.Struct && sf.Type != durationType && sf.Type != reflect.TypeOf(time.Time{}) {
			nested := envPrefix
			if env != "" {
				nested = fullEnv + "_"
			}
			l.walk(fv, path, nested)
			continue
		}

		// Allocate nil struct pointers so their fields are addressable.
		if fv.Kind() == reflect.Pointer && sf.Type.Elem().Kind() == reflect.Struct {
			if fv.IsNil() {
				fv.Set(reflect.New(sf.Type.Elem()))
			}
			l.walk(fv.Elem(), path, envPrefix)
			continue
		}

		l.fields = append(l.fields, &field{
			path:    path,
			value:   fv,
			envName: fullEnv,
			flagRef: sf.Tag.Get(tagFlag),
			defVal:  sf.Tag.Get(tagDefault),
			usage:   sf.Tag.Get(tagUsage),
			secret:  sf.Tag.Get(tagSecret) == "true",
		})
	}
}

// ApplyDefaults writes every `default:"..."` tag into the target.
//
// Runs first so later layers overwrite it, and so a field absent from all layers
// still holds a sensible value rather than a bare zero.
func (l *Loader) ApplyDefaults() error {
	for _, f := range l.fields {
		if f.defVal == "" {
			continue
		}
		if err := setValue(f.value, f.defVal); err != nil {
			return fmt.Errorf("config: default for %s: %w", f.path, err)
		}
	}
	return nil
}

// ApplyFile decodes data into the target using the caller-supplied decoder.
//
// Layer two. The decoder writes only the keys present in the document, so this
// overrides defaults without clobbering untouched fields.
func (l *Loader) ApplyFile(data []byte, decode DecodeFunc, dst any) error {
	if decode == nil {
		return errors.New("config: no decoder supplied for the file layer")
	}
	if len(data) == 0 {
		return nil
	}
	if err := decode(data, dst); err != nil {
		return fmt.Errorf("config: decode file: %w", err)
	}
	return nil
}

// ApplyEnv reads each field's environment variable, if set.
//
// Layer three. Presence is tested with LookupEnv rather than Getenv, so an
// intentionally empty value (BEUVIAN_PROXY="") is respected as an override
// instead of being confused with "unset".
//
// aliases maps a field's fully-prefixed variable name to a fallback variable,
// consulted only when the primary is unset. This exists because hosting platforms
// inject their own conventional names — Railway and Heroku set PORT, not
// BEUVIAN_SERVER_PORT — and those values must participate in the normal
// precedence chain as environment-layer inputs.
//
// The alternative, copying PORT into our namespace with os.Setenv before
// resolving, would work but mutates the process environment as a side effect of
// loading configuration. That makes a second Load behave differently from the
// first and leaks between tests, so the aliasing is done here instead.
func (l *Loader) ApplyEnv(prefix string, aliases map[string]string) error {
	for _, f := range l.fields {
		if f.envName == "" {
			continue
		}
		key := f.envName
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			key = prefix + key
		}

		raw, ok := os.LookupEnv(key)
		if !ok {
			// Fall back to the platform's own name, if one is registered. Checked
			// second so our namespaced variable always wins.
			if alias, has := aliases[key]; has && alias != "" {
				raw, ok = os.LookupEnv(alias)
				if ok {
					key = alias // report the variable the value actually came from
				}
			}
		}
		if !ok {
			continue
		}
		if err := setValue(f.value, raw); err != nil {
			return fmt.Errorf("config: env %s: %w", key, err)
		}
	}
	return nil
}

// BindFlags registers a CLI flag for every field carrying a `flag:"..."` tag.
//
// Flags are registered as strings regardless of the destination type, and parsed
// by setValue later. This is what makes "was it explicitly set?" answerable: a
// typed flag with a default is indistinguishable from an unset one, and the CLI
// layer must only override when the user actually typed something.
func (l *Loader) BindFlags(fs *flag.FlagSet) {
	l.flagSet = fs
	for _, f := range l.fields {
		if f.flagRef == "" {
			continue
		}
		usage := f.usage
		if usage == "" {
			usage = f.path
		}
		if f.defVal != "" {
			usage = fmt.Sprintf("%s (default %q)", usage, f.defVal)
		}
		// Empty flag default: a non-empty one would be indistinguishable from
		// a user-supplied value of the same text.
		holder := fs.String(f.flagRef, "", usage)
		l.flagValues[f.flagRef] = holder
	}
}

// ApplyFlags applies only the flags the user explicitly set.
//
// Layer four, the highest priority. Visit (not VisitAll) is the load-bearing
// detail: it iterates only flags present on the command line, so an untouched
// flag does not silently override the environment or the config file.
func (l *Loader) ApplyFlags() error {
	if l.flagSet == nil {
		return nil
	}
	byName := map[string]*field{}
	for _, f := range l.fields {
		if f.flagRef != "" {
			byName[f.flagRef] = f
		}
	}

	var applyErr error
	l.flagSet.Visit(func(fl *flag.Flag) {
		f, ok := byName[fl.Name]
		if !ok {
			return // a flag owned by the caller, not by the config struct
		}
		if err := setValue(f.value, fl.Value.String()); err != nil && applyErr == nil {
			applyErr = fmt.Errorf("config: flag -%s: %w", fl.Name, err)
		}
	})
	return applyErr
}

// setValue parses s according to v's type and assigns it.
//
// Errors mention the target type, because a config typo ("PORT=eighty") should
// produce a message a user can act on rather than a bare parse failure.
func setValue(v reflect.Value, s string) error {
	// time.Duration before the integer cases: it is an int64 underneath but must
	// accept "30s", not a nanosecond count.
	if v.Type() == durationType {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q (want e.g. 30s, 5m): %w", s, err)
		}
		v.SetInt(int64(d))
		return nil
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString(s)

	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("invalid boolean %q (want true/false): %w", s, err)
		}
		v.SetBool(b)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer %q: %w", s, err)
		}
		v.SetInt(n)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid unsigned integer %q: %w", s, err)
		}
		v.SetUint(n)

	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid number %q: %w", s, err)
		}
		v.SetFloat(n)

	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice element type %s", v.Type().Elem())
		}
		// Comma-separated, trimmed, empties dropped. Chosen because it is what
		// both shells and Railway/Vercel environment editors handle cleanly;
		// CORS origin lists are the main consumer.
		var out []string
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		v.Set(reflect.ValueOf(out))

	default:
		return fmt.Errorf("unsupported config field type %s", v.Type())
	}
	return nil
}
