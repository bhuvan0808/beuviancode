package lifecycle_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/lifecycle"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
)

// recorder tracks start/stop ordering across components.
type recorder struct {
	mu    sync.Mutex
	order []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, s)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func comp(r *recorder, name string, startErr, stopErr error) lifecycle.Component {
	return lifecycle.Func{
		ComponentName: name,
		OnStart: func(context.Context) error {
			r.add("start:" + name)
			return startErr
		},
		OnStop: func(context.Context) error {
			r.add("stop:" + name)
			return stopErr
		},
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestShutdownIsReverseOfStartup(t *testing.T) {
	// The property that makes deploys safe: the HTTP server must stop accepting
	// requests before the database pool it depends on is closed.
	r := &recorder{}
	s := lifecycle.New(blog.Discard(), time.Second)
	s.Add(
		comp(r, "database", nil, nil),
		comp(r, "redis", nil, nil),
		comp(r, "httpserver", nil, nil),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give startup a moment, then request shutdown.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	want := []string{
		"start:database", "start:redis", "start:httpserver",
		"stop:httpserver", "stop:redis", "stop:database",
	}
	if got := r.snapshot(); !equal(got, want) {
		t.Errorf("ordering wrong:\n got %v\nwant %v", got, want)
	}
}

func TestFailedStartUnwindsAlreadyStartedComponents(t *testing.T) {
	// A half-booted process must not leak a bound port or an open pool.
	r := &recorder{}
	boom := errors.New("redis unreachable")
	s := lifecycle.New(blog.Discard(), time.Second)
	s.Add(
		comp(r, "database", nil, nil),
		comp(r, "redis", boom, nil),
		comp(r, "httpserver", nil, nil), // must never start
	)

	err := s.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the start failure", err)
	}

	got := r.snapshot()
	want := []string{"start:database", "start:redis", "stop:database"}
	if !equal(got, want) {
		t.Errorf("unwind wrong:\n got %v\nwant %v", got, want)
	}
	for _, ev := range got {
		if ev == "start:httpserver" {
			t.Error("components after the failing one must not start")
		}
	}
}

func TestFatalErrorTriggersGracefulShutdown(t *testing.T) {
	r := &recorder{}
	s := lifecycle.New(blog.Discard(), time.Second)
	s.Add(comp(r, "database", nil, nil), comp(r, "gateway", nil, nil))

	crash := errors.New("websocket gateway died")
	go func() {
		time.Sleep(30 * time.Millisecond)
		s.Fatal(crash)
	}()

	err := s.Run(context.Background())
	if !errors.Is(err, crash) {
		t.Fatalf("err = %v, want the fatal error", err)
	}
	// Even on a crash path, components must be stopped in reverse order.
	want := []string{"start:database", "start:gateway", "stop:gateway", "stop:database"}
	if got := r.snapshot(); !equal(got, want) {
		t.Errorf("ordering wrong:\n got %v\nwant %v", got, want)
	}
}

func TestStopErrorsAreReportedNotSwallowed(t *testing.T) {
	r := &recorder{}
	stopBoom := errors.New("pool close failed")
	s := lifecycle.New(blog.Discard(), time.Second)
	s.Add(comp(r, "database", nil, stopBoom))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()

	err := <-done
	if !errors.Is(err, stopBoom) {
		t.Errorf("err = %v, want the stop error surfaced", err)
	}
}

func TestSlowComponentHitsShutdownTimeout(t *testing.T) {
	// A component that overruns must be reported, otherwise a chronic offender
	// stays invisible until it causes a failed deploy.
	s := lifecycle.New(blog.Discard(), 60*time.Millisecond)
	s.Add(lifecycle.Func{
		ComponentName: "sluggish",
		OnStart:       func(context.Context) error { return nil },
		OnStop: func(ctx context.Context) error {
			<-ctx.Done() // never finishes within the grace period
			return ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, lifecycle.ErrShutdownTimeout) {
			t.Errorf("err = %v, want ErrShutdownTimeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run never returned; the grace period was not enforced")
	}
}

func TestStopReceivesAUsableDeadline(t *testing.T) {
	// Regression guard for the subtle bug: deriving the shutdown context from the
	// already-cancelled parent gives every Stop an expired deadline, so the
	// graceful path is skipped entirely.
	var (
		mu       sync.Mutex
		hadTime  bool
		wasAlive bool
	)
	s := lifecycle.New(blog.Discard(), 2*time.Second)
	s.Add(lifecycle.Func{
		ComponentName: "checker",
		OnStart:       func(context.Context) error { return nil },
		OnStop: func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			wasAlive = ctx.Err() == nil
			if dl, ok := ctx.Deadline(); ok {
				hadTime = time.Until(dl) > time.Second
			}
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel() // parent is now cancelled

	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !wasAlive {
		t.Error("Stop received an already-cancelled context; graceful shutdown would be skipped")
	}
	if !hadTime {
		t.Error("Stop did not receive the full grace period as its deadline")
	}
}

func TestNoComponentsIsNotAnError(t *testing.T) {
	s := lifecycle.New(blog.Discard(), time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context error", err)
	}
}

func TestFatalIgnoresNilAndDoesNotBlockWhenFull(t *testing.T) {
	s := lifecycle.New(blog.Discard(), time.Second)
	s.Fatal(nil) // must be a no-op, not a shutdown trigger
	// Overfill the buffer; Fatal must never block the calling component.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.Fatal(errors.New("flood"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Fatal blocked; a failing component would hang instead of reporting")
	}
}
