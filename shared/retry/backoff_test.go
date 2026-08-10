package retry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/retry"
)

func TestNextGrowsExponentiallyAndRespectsMax(t *testing.T) {
	p := retry.Policy{
		Initial:    100 * time.Millisecond,
		Max:        2 * time.Second,
		Multiplier: 2.0,
		Jitter:     0, // deterministic for this assertion
	}
	b := retry.New(p)
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		2 * time.Second, // capped
		2 * time.Second,
	}
	for i, w := range want {
		if got := b.Next(); got != w {
			t.Errorf("attempt %d: got %v, want %v", i, got, w)
		}
	}
}

func TestNextNeverExceedsMaxOrGoesNegative(t *testing.T) {
	// Regression guard: a large attempt count overflows the float->Duration
	// conversion, which produced a negative delay and an immediate retry storm.
	b := retry.New(retry.Policy{
		Initial:    time.Second,
		Max:        30 * time.Second,
		Multiplier: 3.0,
		Jitter:     0.3,
	})
	for i := 0; i < 500; i++ {
		d := b.Next()
		if d < 0 {
			t.Fatalf("attempt %d produced a negative delay: %v", i, d)
		}
		if d > 30*time.Second {
			t.Fatalf("attempt %d exceeded Max: %v", i, d)
		}
	}
}

func TestJitterSpreadsDelays(t *testing.T) {
	// Without jitter, every agent reconnects in lockstep after a backend
	// restart and the herd keeps the gateway down. Assert it actually varies.
	p := retry.Policy{Initial: time.Second, Max: time.Second, Multiplier: 1.0, Jitter: 0.5}
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 50; i++ {
		seen[retry.New(p).Next()] = struct{}{}
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct delays across 50 samples; jitter is not spreading load", len(seen))
	}
}

func TestResetClearsAttemptCounter(t *testing.T) {
	b := retry.New(retry.Policy{Initial: 100 * time.Millisecond, Max: time.Minute, Multiplier: 2, Jitter: 0})
	b.Next()
	b.Next()
	b.Next()
	if b.Attempt() != 3 {
		t.Fatalf("Attempt = %d, want 3", b.Attempt())
	}
	b.Reset()
	if b.Attempt() != 0 {
		t.Errorf("Attempt after Reset = %d, want 0", b.Attempt())
	}
	if got := b.Next(); got != 100*time.Millisecond {
		t.Errorf("first delay after Reset = %v, want the initial delay", got)
	}
}

func TestExhaustedOnlyWhenBounded(t *testing.T) {
	unbounded := retry.New(retry.ReconnectPolicy())
	for i := 0; i < 100; i++ {
		unbounded.Next()
	}
	if unbounded.Exhausted() {
		t.Error("the reconnect policy must never exhaust; a laptop closed for the weekend must still reconnect")
	}

	bounded := retry.New(retry.Policy{Initial: time.Millisecond, Max: time.Second, Multiplier: 2, MaxAttempts: 3})
	for i := 0; i < 3; i++ {
		if bounded.Exhausted() {
			t.Fatalf("exhausted early at attempt %d", i)
		}
		bounded.Next()
	}
	if !bounded.Exhausted() {
		t.Error("expected exhaustion after MaxAttempts")
	}
}

func TestDoSucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := retry.Do(context.Background(),
		retry.Policy{Initial: time.Millisecond, Max: 5 * time.Millisecond, Multiplier: 2, MaxAttempts: 5},
		func(context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("connection reset")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestDoStopsImmediatelyOnPermanentError(t *testing.T) {
	// An invalid device token must not be retried: retrying cannot help and it
	// amplifies load against our own gateway.
	sentinel := errors.New("unauthorised")
	calls := 0
	err := retry.Do(context.Background(),
		retry.Policy{Initial: time.Millisecond, Max: time.Second, Multiplier: 2, MaxAttempts: 10},
		func(context.Context) error {
			calls++
			return retry.Fatal(sentinel)
		})
	if calls != 1 {
		t.Errorf("calls = %d, want 1 — a permanent error must not be retried", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to unwrap to the sentinel", err)
	}
}

func TestDoReportsExhaustionWithLastError(t *testing.T) {
	last := errors.New("dial tcp: i/o timeout")
	err := retry.Do(context.Background(),
		retry.Policy{Initial: time.Millisecond, Max: 2 * time.Millisecond, Multiplier: 2, MaxAttempts: 3},
		func(context.Context) error { return last })
	if !errors.Is(err, retry.ErrExhausted) {
		t.Errorf("err = %v, want ErrExhausted", err)
	}
	// The caller needs to know *why* it failed, not merely that it did.
	if !errors.Is(err, last) {
		t.Errorf("err = %v, want the last observed error to be retained", err)
	}
}

func TestDoAbortsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := retry.Do(ctx,
		retry.ReconnectPolicy(), // unbounded: only cancellation can end this
		func(context.Context) error {
			calls++
			return errors.New("still down")
		})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls == 0 {
		t.Error("expected at least one attempt before cancellation")
	}
}

func TestWaitReturnsPromptlyOnCancellation(t *testing.T) {
	// Shutdown must not block for the full backoff delay.
	b := retry.New(retry.Policy{Initial: 10 * time.Second, Max: 10 * time.Second, Multiplier: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := b.Wait(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("Wait should return the context error")
	}
	if elapsed > time.Second {
		t.Errorf("Wait blocked for %v after cancellation; shutdown would stall", elapsed)
	}
}
