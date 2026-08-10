// Package lifecycle supervises ordered startup and graceful shutdown.
//
// Both Beuvian binaries need the same thing: bring dependencies up in order, run
// until a signal or a fatal error arrives, then tear down in reverse order within
// a bounded grace period. Writing that twice would guarantee the two drift, and
// shutdown bugs are exactly the kind that only appear in production.
//
// Reverse-order shutdown is the important property. The backend must stop
// accepting HTTP requests before it closes the database pool, or in-flight
// requests fail during every deploy. Encoding the ordering in the supervisor
// means a component author cannot get it wrong by accident.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Component is a supervised unit with a bounded lifecycle.
//
// Start must NOT block. A component that runs a loop starts a goroutine and
// returns; if that loop dies it reports the failure through the Fatal channel it
// was given. Blocking here would deadlock the supervisor before later components
// ever started, which is a mistake that is easy to make and hard to diagnose.
type Component interface {
	// Name identifies the component in logs. Keep it short and stable.
	Name() string

	// Start brings the component up. Returning an error aborts startup and
	// unwinds whatever already started.
	Start(ctx context.Context) error

	// Stop shuts the component down. The context carries the remaining grace
	// period; Stop must respect it and return promptly when it expires.
	Stop(ctx context.Context) error
}

// Func builds a Component from plain functions, so a small piece of setup does
// not require declaring a type.
type Func struct {
	ComponentName string
	OnStart       func(ctx context.Context) error
	OnStop        func(ctx context.Context) error
}

// Name returns the component name, used in logs and error messages.
func (f Func) Name() string { return f.ComponentName }

// Start invokes OnStart, or succeeds immediately when it is nil.
func (f Func) Start(ctx context.Context) error {
	if f.OnStart == nil {
		return nil
	}
	return f.OnStart(ctx)
}

// Stop invokes OnStop, or succeeds immediately when it is nil.
func (f Func) Stop(ctx context.Context) error {
	if f.OnStop == nil {
		return nil
	}
	return f.OnStop(ctx)
}

// DefaultGrace is the shutdown budget when none is configured.
//
// 15s comfortably covers draining in-flight HTTP requests and closing pools,
// while staying under the 30s that Railway and most orchestrators allow between
// SIGTERM and SIGKILL. Exceeding the platform's window means being killed
// mid-drain, which defeats the point of graceful shutdown.
const DefaultGrace = 15 * time.Second

// Supervisor runs a set of components through one lifecycle.
type Supervisor struct {
	log        *slog.Logger
	grace      time.Duration
	components []Component
	fatal      chan error
	signals    []os.Signal
}

// New returns a Supervisor.
//
// The logger is injected rather than package-global so tests can assert on
// shutdown ordering, and so the supervisor inherits the caller's correlation
// fields instead of logging into a separate void.
func New(log *slog.Logger, grace time.Duration) *Supervisor {
	if grace <= 0 {
		grace = DefaultGrace
	}
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return &Supervisor{
		log:   log,
		grace: grace,
		// Buffered so a component reporting a fatal error is never blocked by a
		// supervisor that is already shutting down for another reason.
		fatal:   make(chan error, 8),
		signals: []os.Signal{syscall.SIGINT, syscall.SIGTERM},
	}
}

// Add appends components. Startup order is registration order; shutdown is the
// reverse, so register dependencies before their dependents.
func (s *Supervisor) Add(components ...Component) {
	s.components = append(s.components, components...)
}

// Fatal reports an unrecoverable failure from a running component, triggering
// graceful shutdown of the whole process.
//
// Non-blocking: if the buffer is full the supervisor is already terminating, and
// the additional error would not change the outcome.
func (s *Supervisor) Fatal(err error) {
	if err == nil {
		return
	}
	select {
	case s.fatal <- err:
	default:
		s.log.Warn("fatal error dropped; shutdown already in progress",
			slog.String("error", err.Error()))
	}
}

// ErrShutdownTimeout means one or more components did not stop within the grace
// period. Reported rather than swallowed: a component that routinely overruns is
// a real bug that would otherwise stay invisible.
var ErrShutdownTimeout = errors.New("lifecycle: shutdown exceeded grace period")

// Run starts every component, blocks until termination is requested, then shuts
// down in reverse order.
//
// It returns nil for a clean shutdown, or the triggering error otherwise.
// Termination is requested by ctx cancellation, SIGINT/SIGTERM, or Fatal.
func (s *Supervisor) Run(ctx context.Context) error {
	// Intercept signals before starting anything, so a Ctrl-C during a slow
	// startup is honoured rather than killing the process mid-initialisation.
	sigCtx, stopSignals := signal.NotifyContext(ctx, s.signals...)
	defer stopSignals()

	started := make([]Component, 0, len(s.components))
	for _, c := range s.components {
		s.log.Info("starting component", slog.String("component", c.Name()))
		if err := c.Start(sigCtx); err != nil {
			startErr := fmt.Errorf("lifecycle: start %s: %w", c.Name(), err)
			s.log.Error("component failed to start",
				slog.String("component", c.Name()), slog.String("error", err.Error()))
			// Unwind what already started, so a failed boot does not leak
			// connections or leave a port bound.
			if stopErr := s.shutdown(sigCtx, started); stopErr != nil {
				return errors.Join(startErr, stopErr)
			}
			return startErr
		}
		started = append(started, c)
	}

	s.log.Info("all components started", slog.Int("count", len(started)))

	var cause error
	select {
	case <-sigCtx.Done():
		// Distinguish an operator-initiated stop from a parent cancellation:
		// only the latter is an error worth propagating.
		if err := ctx.Err(); err != nil {
			cause = err
			s.log.Info("shutting down: context cancelled")
		} else {
			s.log.Info("shutting down: signal received")
		}
	case err := <-s.fatal:
		cause = err
		s.log.Error("shutting down: fatal component error", slog.String("error", err.Error()))
	}

	if err := s.shutdown(sigCtx, started); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// shutdown stops components in reverse registration order.
//
// The parent context is usually already cancelled by the time we get here, so the
// derived stop deadlines come from a fresh base: context.WithoutCancel strips
// cancellation while keeping any values. Deriving from the cancelled parent would
// give every Stop an expired deadline and skip the graceful path entirely — the
// exact bug graceful shutdown exists to avoid.
func (s *Supervisor) shutdown(ctx context.Context, started []Component) error {
	base := context.WithoutCancel(ctx)
	deadline := time.Now().Add(s.grace)

	var errs []error
	for i := len(started) - 1; i >= 0; i-- {
		c := started[i]

		remaining := time.Until(deadline)
		if remaining <= 0 {
			errs = append(errs, fmt.Errorf("%w: %s never got the chance to stop",
				ErrShutdownTimeout, c.Name()))
			continue
		}

		// Each component gets whatever remains of the shared budget, so one slow
		// component cannot extend total shutdown past the platform's SIGKILL
		// deadline.
		stopCtx, cancel := context.WithTimeout(base, remaining)
		start := time.Now()
		err := c.Stop(stopCtx)
		cancel()

		took := time.Since(start)
		switch {
		case err == nil:
			s.log.Info("component stopped",
				slog.String("component", c.Name()),
				slog.Duration("took", took))
		case errors.Is(err, context.DeadlineExceeded):
			errs = append(errs, fmt.Errorf("%w: %s", ErrShutdownTimeout, c.Name()))
			s.log.Error("component exceeded its shutdown budget",
				slog.String("component", c.Name()), slog.Duration("took", took))
		default:
			errs = append(errs, fmt.Errorf("lifecycle: stop %s: %w", c.Name(), err))
			s.log.Error("component failed to stop cleanly",
				slog.String("component", c.Name()),
				slog.String("error", err.Error()))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	s.log.Info("shutdown complete")
	return nil
}
