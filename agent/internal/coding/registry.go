package coding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// Factory constructs an Adapter.
//
// The logger is passed in rather than created inside, so an adapter's output
// carries the session's correlation fields instead of logging into a separate
// stream that cannot be joined to the rest of a trace.
type Factory func(logger *slog.Logger) Adapter

// Registry maps adapter names to their constructors.
//
// A registry rather than a switch statement in the session manager: adding
// Codex CLI must not require editing session code. A new adapter is one file
// plus one Register call, which is what PROJECT.md means by "minimal code
// changes".
//
// Deliberately an instance type rather than a package-level global. A global
// would be mutable shared state (a coding standard PROJECT.md rules out) and
// would let tests leak registrations into one another.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	detectors map[string]Detector
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]Factory),
		detectors: make(map[string]Detector),
	}
}

// Register adds an adapter factory and its optional detector.
//
// Returns an error rather than panicking on a duplicate name: registration is
// driven by configuration in some future plugin scenario, and a panic there
// would take down a user's agent over a recoverable conflict.
func (r *Registry) Register(name string, f Factory, d Detector) error {
	if name == "" {
		return fmt.Errorf("coding: adapter name must not be empty")
	}
	if f == nil {
		return fmt.Errorf("coding: adapter %q has a nil factory", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("coding: adapter %q is already registered", name)
	}
	r.factories[name] = f
	if d != nil {
		r.detectors[name] = d
	}
	return nil
}

// MustRegister is Register for package initialisation, where a duplicate name is
// a programming error that should fail the build's tests immediately.
func (r *Registry) MustRegister(name string, f Factory, d Detector) {
	if err := r.Register(name, f, d); err != nil {
		panic(err)
	}
}

// New constructs the named adapter.
func (r *Registry) New(name string, logger *slog.Logger) (Adapter, error) {
	r.mu.RLock()
	f, ok := r.factories[name]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %v)", ErrUnsupportedAdapter, name, r.Names())
	}
	return f(logger), nil
}

// Names returns the registered adapter names, sorted for stable output in logs,
// help text, and the AUTH capability list.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.factories))
	for name := range r.factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Has reports whether name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[name]
	return ok
}

// DetectAll runs every registered detector and returns those that are installed.
//
// Errors other than ErrNotInstalled are collected per adapter rather than
// aborting the sweep: one broken detector must not hide the others, or a single
// misbehaving tool would make the device look like it has nothing installed.
func (r *Registry) DetectAll(ctx context.Context) (map[string]Installation, map[string]error) {
	r.mu.RLock()
	detectors := make(map[string]Detector, len(r.detectors))
	for name, d := range r.detectors {
		detectors[name] = d
	}
	r.mu.RUnlock()

	found := make(map[string]Installation)
	problems := make(map[string]error)

	for name, d := range detectors {
		if err := ctx.Err(); err != nil {
			problems[name] = err
			continue
		}
		inst, err := d.Detect(ctx)
		switch {
		case err == nil:
			found[name] = inst
		case errors.Is(err, ErrNotInstalled):
			// Absence is an ordinary outcome, not a problem worth reporting.
		default:
			problems[name] = err
		}
	}
	return found, problems
}

// Capabilities returns the sorted names of adapters actually installed on this
// machine, for the AUTH handshake.
//
// The backend uses this to avoid dispatching a prompt to a device that cannot
// service it, so it must reflect what is installed rather than what is compiled
// in — a build supporting five adapters on a machine with one installed has one
// capability.
func (r *Registry) Capabilities(ctx context.Context) []string {
	found, _ := r.DetectAll(ctx)
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
