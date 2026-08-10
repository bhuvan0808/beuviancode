// Package retry implements exponential backoff with jitter.
//
// PROJECT.md requires exponential backoff in three places: agent WebSocket
// reconnection, failed HTTP request retries, and prompt redelivery. One
// implementation serves all three, so the retry semantics cannot drift between
// them and there is a single place to reason about worst-case load.
package retry

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/id"
)

// Policy declares a backoff schedule.
//
// A value type with no internal state, so one Policy can be shared by many
// concurrent Backoff instances without locking. Mutable attempt counters live in
// Backoff, not here.
type Policy struct {
	// Initial is the delay before the first retry.
	Initial time.Duration
	// Max caps any single delay. Without a cap, exponential growth reaches
	// absurd delays (hours) and an agent appears permanently dead.
	Max time.Duration
	// Multiplier is the growth factor per attempt.
	Multiplier float64
	// Jitter is the fraction of the computed delay (0..1) randomised away.
	//
	// Non-zero jitter is load-bearing, not decorative: if the backend restarts,
	// every connected agent reconnects on an identical schedule and the
	// synchronised retries become a self-inflicted thundering herd that keeps
	// the gateway down. Jitter spreads them out.
	Jitter float64
	// MaxAttempts bounds total attempts; zero means retry forever.
	//
	// The agent's reconnect loop uses zero deliberately — a laptop that was shut
	// for the weekend must still reconnect on Monday rather than having given up.
	MaxAttempts int
}

// DefaultPolicy is used for HTTP request retries: quick, bounded, gives up so a
// caller is not blocked indefinitely.
func DefaultPolicy() Policy {
	return Policy{
		Initial:     200 * time.Millisecond,
		Max:         10 * time.Second,
		Multiplier:  2.0,
		Jitter:      0.2,
		MaxAttempts: 5,
	}
}

// ReconnectPolicy is used for the agent's WebSocket reconnect loop: unbounded
// attempts, and a 30s ceiling so a recovered backend is noticed promptly.
func ReconnectPolicy() Policy {
	return Policy{
		Initial:     500 * time.Millisecond,
		Max:         30 * time.Second,
		Multiplier:  1.8,
		Jitter:      0.3,
		MaxAttempts: 0, // never stop trying
	}
}

// Backoff tracks attempt state for one retry sequence.
//
// Not safe for concurrent use by design: each retry sequence is owned by exactly
// one goroutine, and adding a mutex would imply a sharing pattern that would be a
// bug if it existed.
type Backoff struct {
	policy  Policy
	attempt int
}

// New returns a Backoff for the given policy.
func New(p Policy) *Backoff {
	if p.Initial <= 0 {
		p.Initial = 100 * time.Millisecond
	}
	if p.Max <= 0 {
		p.Max = 30 * time.Second
	}
	if p.Multiplier < 1 {
		p.Multiplier = 2.0
	}
	if p.Jitter < 0 {
		p.Jitter = 0
	}
	if p.Jitter > 1 {
		p.Jitter = 1
	}
	return &Backoff{policy: p}
}

// Attempt returns how many delays have been handed out so far.
func (b *Backoff) Attempt() int { return b.attempt }

// Reset clears the attempt counter after a success.
//
// Must be called on every successful connection, otherwise a long-lived agent
// that reconnects occasionally accumulates attempts and eventually waits the
// maximum delay after a single blip.
func (b *Backoff) Reset() { b.attempt = 0 }

// Exhausted reports whether MaxAttempts has been reached.
func (b *Backoff) Exhausted() bool {
	return b.policy.MaxAttempts > 0 && b.attempt >= b.policy.MaxAttempts
}

// Next returns the next delay and advances the attempt counter.
func (b *Backoff) Next() time.Duration {
	// Compute from the pre-increment attempt so the first delay is Initial.
	raw := float64(b.policy.Initial) * math.Pow(b.policy.Multiplier, float64(b.attempt))
	b.attempt++

	// Guard the float->Duration conversion: at high attempt counts raw exceeds
	// the int64 range and the conversion is implementation-defined, which
	// previously showed up as a negative duration and an immediate retry storm.
	if raw > float64(math.MaxInt64) || raw > float64(b.policy.Max) {
		raw = float64(b.policy.Max)
	}
	d := time.Duration(raw)

	if b.policy.Jitter > 0 {
		// Symmetric jitter in [-J, +J] around d, drawn from the CSPRNG.
		frac := float64(id.Uint64()%2001)/1000.0 - 1.0 // [-1, 1]
		d += time.Duration(float64(d) * b.policy.Jitter * frac)
	}
	if d < 0 {
		d = 0
	}
	if d > b.policy.Max {
		d = b.policy.Max
	}
	return d
}

// Wait sleeps for the next delay, or returns early if ctx is cancelled.
//
// Context-aware so shutdown is immediate: a naive time.Sleep would keep the
// process alive for up to Max seconds after a termination signal.
func (b *Backoff) Wait(ctx context.Context) error {
	d := b.Next()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrExhausted is returned by Do when every attempt failed.
var ErrExhausted = errors.New("retry: attempts exhausted")

// Permanent wraps an error to stop retrying immediately.
//
// This is how a caller distinguishes "the network blipped, try again" from "the
// credentials are wrong, retrying cannot help". Without it, Do would retry
// unauthorised requests until exhaustion, wasting time and amplifying load.
type Permanent struct{ Err error }

func (p *Permanent) Error() string { return p.Err.Error() }
func (p *Permanent) Unwrap() error { return p.Err }

// Fatal marks err as non-retryable.
func Fatal(err error) error { return &Permanent{Err: err} }

// Do runs fn until it succeeds, returns a Permanent error, exhausts the policy,
// or ctx is cancelled. The last observed error is wrapped into the result so the
// caller learns why it ultimately failed, not merely that it did.
func Do(ctx context.Context, p Policy, fn func(ctx context.Context) error) error {
	b := New(p)
	var last error
	for {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return errors.Join(err, last)
			}
			return err
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}

		var perm *Permanent
		if errors.As(err, &perm) {
			return perm.Err
		}
		last = err

		if b.Exhausted() {
			return errors.Join(ErrExhausted, last)
		}
		if werr := b.Wait(ctx); werr != nil {
			return errors.Join(werr, last)
		}
	}
}
