// Package power keeps the machine awake while Beuvian controls a coding session.
//
// This exists because of a specific failure mode: a developer starts a 45-minute
// task, closes the laptop lid or walks away, the machine sleeps, and the coding
// agent stops. From the phone it looks like the task hung. Preventing sleep for
// exactly the duration of an active session is what makes remote control
// trustworthy.
//
// PROJECT.md specifies PreventSleep/AllowSleep/Status and requires Windows, macOS,
// and Linux. Each platform gets its own build-tagged file, so the interface has no
// runtime OS branching and a build for any target compiles only the code that can
// run on it.
//
// Phase 1 defines the contract and the cross-platform build structure. Phase 3
// implements the syscalls.
package power

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// State reports whether sleep is currently inhibited.
type State struct {
	// Prevented is true while an inhibition is held.
	Prevented bool

	// Reason is the human-readable justification shown by OS tooling
	// (powercfg /requests on Windows, pmset -g assertions on macOS). Making the
	// reason visible to the user's own OS tools is deliberate: software that
	// silently prevents sleep is indistinguishable from a bug.
	Reason string

	// Since is when the inhibition was acquired; zero when not prevented.
	Since time.Time

	// Supported is false on platforms with no implementation, so a caller can
	// tell "not inhibited" from "cannot inhibit here".
	Supported bool
}

// Held returns how long the inhibition has been active.
func (s State) Held() time.Duration {
	if s.Since.IsZero() {
		return 0
	}
	return time.Since(s.Since)
}

// Manager controls the platform's sleep inhibition.
//
// Implementations must be idempotent and reference-safe: calling PreventSleep
// twice then AllowSleep once must leave sleep allowed. Leaking an inhibition
// drains a user's battery indefinitely, which is a worse bug than failing to
// acquire one, so the accounting is kept simple and single-owner rather than
// reference-counted.
type Manager interface {
	// PreventSleep inhibits sleep. Idempotent: a second call with a new reason
	// updates the reason without acquiring a second inhibition.
	PreventSleep(reason string) error

	// AllowSleep releases the inhibition. Safe to call when none is held, so
	// cleanup paths need no state checks.
	AllowSleep() error

	// Status reports the current state.
	Status() State
}

// ErrUnsupported means this platform has no sleep-inhibition implementation.
//
// Callers treat it as a warning, not a fatal error: a session on an unsupported
// platform should still run, with the user told that sleep cannot be prevented.
var ErrUnsupported = errors.New("power: sleep inhibition is not supported on this platform")

// New returns the Manager for the current platform.
//
// Implemented per-OS in the build-tagged files alongside this one, so selection
// happens at compile time rather than through a runtime.GOOS switch.
func New(logger *slog.Logger) Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return newPlatformManager(logger.With(slog.String("component", "power")))
}

// base holds the state tracking common to every platform, so each
// implementation contributes only its syscalls.
//
// Embedded rather than inherited-from: the platform type composes base and
// overrides only what it must, which is the composition-over-inheritance rule
// PROJECT.md asks for.
type base struct {
	mu        sync.Mutex
	prevented bool
	reason    string
	since     time.Time
	supported bool
	logger    *slog.Logger
}

func (b *base) status() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return State{
		Prevented: b.prevented,
		Reason:    b.reason,
		Since:     b.since,
		Supported: b.supported,
	}
}

// markPrevented records a successful acquisition.
func (b *base) markPrevented(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.prevented {
		b.since = time.Now()
	}
	b.prevented = true
	b.reason = reason
}

// markAllowed records a release.
func (b *base) markAllowed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prevented = false
	b.reason = ""
	b.since = time.Time{}
}

// unsupportedManager is used on platforms without an implementation.
//
// It records state and logs, so behaviour is observable and the session manager
// needs no special case. Failing loudly here would block sessions on platforms
// that are otherwise perfectly usable.
type unsupportedManager struct {
	base
	goos string
}

// PreventSleep records the attempt and warns, then reports the platform
// limitation so callers can degrade instead of pretending.
func (m *unsupportedManager) PreventSleep(reason string) error {
	m.markPrevented(reason)
	m.logger.Warn("cannot prevent sleep on this platform; the session may be interrupted if the machine sleeps",
		slog.String("platform", m.goos), slog.String("reason", reason))
	return fmt.Errorf("%w: %s", ErrUnsupported, m.goos)
}

// AllowSleep releases the inhibition state unconditionally.
func (m *unsupportedManager) AllowSleep() error {
	m.markAllowed()
	return nil
}

// Status reports the recorded inhibition state.
func (m *unsupportedManager) Status() State { return m.status() }
