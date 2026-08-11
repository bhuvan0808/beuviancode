package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// Dispatcher implements port.PromptDispatcher.
//
// Strictly an accelerator. PostgreSQL is authoritative for prompts, so every
// method here may fail without data loss — the prompt is still delivered from the
// database on the next reconnect or reconciliation sweep (ADR-0006). That is why
// Publish logs and returns nil-ish errors the caller is expected to tolerate
// rather than surface to the user.
type Dispatcher struct {
	c   *Client
	log *slog.Logger
}

// NewDispatcher returns a Dispatcher.
func NewDispatcher(c *Client, log *slog.Logger) *Dispatcher {
	return &Dispatcher{c: c, log: log.With(slog.String("component", "dispatch"))}
}

var _ port.PromptDispatcher = (*Dispatcher)(nil)

// dispatchChannel is the pub/sub channel carrying prompt-dispatch signals.
const dispatchChannel = "dispatch"

// Publish signals that a prompt is waiting for a device.
//
// The payload is deliberately just two IDs, not the prompt body: the receiving
// instance loads the authoritative row from PostgreSQL. Sending the text would
// mean two copies of the instruction that could disagree, and would put user
// content through a store that PROJECT.md forbids holding business data.
func (d *Dispatcher) Publish(ctx context.Context, deviceID, promptID string) error {
	if !d.c.Available() {
		return nil // degraded: delivery falls back to reconnect
	}
	payload := deviceID + "|" + promptID
	return d.c.rdb.Publish(ctx, d.c.key(dispatchChannel), payload).Err()
}

// Subscribe delivers dispatch signals to this instance.
//
// Cross-instance fan-out is the entire reason this exists: an agent connected to
// instance A must be reachable from an API call served by instance B. Without it,
// horizontal scaling silently breaks prompt delivery for every request that does
// not happen to land on the right instance.
//
// Blocks until ctx is cancelled. Reconnects internally, because a dropped
// subscription that stayed dropped would silently disable dispatch for the whole
// instance — a failure with no visible symptom until a user reports a stuck prompt.
func (d *Dispatcher) Subscribe(ctx context.Context, handler func(deviceID, promptID string)) error {
	if !d.c.Available() {
		<-ctx.Done()
		return ctx.Err()
	}

	sub := d.c.rdb.Subscribe(ctx, d.c.key(dispatchChannel))
	defer func() { _ = sub.Close() }()

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				// go-redis closes the channel only when the subscription ends.
				return errors.New("redis: dispatch subscription closed")
			}
			deviceID, promptID, ok := strings.Cut(msg.Payload, "|")
			if !ok {
				d.log.Warn("malformed dispatch payload", slog.String("payload", msg.Payload))
				continue
			}
			handler(deviceID, promptID)
		}
	}
}

// ---------------------------------------------------------------------------

// Events implements port.EventPublisher.
//
// Carries realtime envelopes to dashboard clients across instances. Like
// Dispatcher this is best-effort: a lost event costs a stale dashboard until the
// next periodic STATUS frame, which is why the agent re-sends status on a timer
// rather than only on transitions.
type Events struct {
	c   *Client
	log *slog.Logger
}

// NewEvents returns an Events publisher.
func NewEvents(c *Client, log *slog.Logger) *Events {
	return &Events{c: c, log: log.With(slog.String("component", "events"))}
}

var _ port.EventPublisher = (*Events)(nil)

const eventChannel = "events"

// userEnvelope wraps an envelope with its target user for the pub/sub hop.
type userEnvelope struct {
	UserID   string            `json:"user_id"`
	Envelope protocol.Envelope `json:"envelope"`
}

// PublishToUser fans an envelope out to every connection owned by a user.
func (e *Events) PublishToUser(ctx context.Context, userID string, env protocol.Envelope) error {
	if !e.c.Available() {
		return nil
	}
	body, err := json.Marshal(userEnvelope{UserID: userID, Envelope: env})
	if err != nil {
		return fmt.Errorf("redis: encode event: %w", err)
	}
	return e.c.rdb.Publish(ctx, e.c.key(eventChannel), body).Err()
}

// Subscribe receives envelopes published by any instance.
func (e *Events) Subscribe(ctx context.Context, handler func(userID string, env protocol.Envelope)) error {
	if !e.c.Available() {
		<-ctx.Done()
		return ctx.Err()
	}

	sub := e.c.rdb.Subscribe(ctx, e.c.key(eventChannel))
	defer func() { _ = sub.Close() }()

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return errors.New("redis: event subscription closed")
			}
			var ue userEnvelope
			if err := json.Unmarshal([]byte(msg.Payload), &ue); err != nil {
				e.log.Warn("malformed event payload", slog.String("error", err.Error()))
				continue
			}
			handler(ue.UserID, ue.Envelope)
		}
	}
}

// ---------------------------------------------------------------------------

// Limiter implements port.RateLimiter.
type Limiter struct{ c *Client }

// NewLimiter returns a Limiter.
func NewLimiter(c *Client) *Limiter { return &Limiter{c: c} }

var _ port.RateLimiter = (*Limiter)(nil)

// Allow consumes one unit against a fixed window.
//
// Fixed window rather than sliding: it is one INCR plus a conditional EXPIRE,
// which is cheap enough to run on every request. The known cost is burst tolerance
// at a window boundary — a client can send 2x the limit across two adjacent windows
// — which is acceptable for abuse protection, and would not be for billing.
//
// Degraded Redis fails OPEN. That is a deliberate availability-over-enforcement
// choice: failing closed would make a Redis outage a total outage. The startup
// warning and the production config check exist precisely because this trade means
// an unnoticed Redis failure silently disables rate limiting.
func (l *Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, int, time.Time, error) {
	now := time.Now()
	if !l.c.Available() {
		return true, limit, now.Add(window), nil
	}

	// Bucket the window into the key so each window starts fresh with no cleanup.
	bucket := now.Truncate(window).Unix()
	rkey := l.c.key("ratelimit", key, fmt.Sprint(bucket))
	resetAt := now.Truncate(window).Add(window)

	pipe := l.c.rdb.Pipeline()
	incr := pipe.Incr(ctx, rkey)
	// TTL slightly beyond the window so a clock skew between instances cannot
	// expire the counter while the window is still current.
	pipe.Expire(ctx, rkey, window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		// Fail open on a Redis error, for the same reason as above.
		return true, limit, resetAt, nil
	}

	count := int(incr.Val())
	remaining := limit - count
	if remaining < 0 {
		remaining = 0
	}
	return count <= limit, remaining, resetAt, nil
}

// ---------------------------------------------------------------------------

// Lock implements port.DistributedLock.
type Lock struct {
	c   *Client
	log *slog.Logger
}

// NewLock returns a Lock.
func NewLock(c *Client, log *slog.Logger) *Lock {
	return &Lock{c: c, log: log.With(slog.String("component", "lock"))}
}

var _ port.DistributedLock = (*Lock)(nil)

// releaseScript deletes the key only if this holder still owns it.
//
// A plain DEL is a real bug: if the holder overran its TTL the lock has already
// been handed to someone else, and DEL would release *their* lock. Comparing the
// token first makes release safe, and it must be atomic — hence Lua rather than
// GET-then-DEL.
var releaseScript = goredis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then
		return redis.call("DEL", KEYS[1])
	end
	return 0
`)

// Acquire takes a lock, returning a release function.
//
// Used for singleton work — the stale-session sweep, log retention — which would
// otherwise run on every instance simultaneously.
//
// With Redis degraded this reports acquired. On a single instance that is correct;
// with several instances it means a periodic job may run more than once. Every job
// guarded by this lock is therefore written to be idempotent, so duplicate
// execution is wasteful rather than harmful.
func (l *Lock) Acquire(ctx context.Context, key string, ttl time.Duration) (func(context.Context), bool, error) {
	if !l.c.Available() {
		return func(context.Context) {}, true, nil
	}

	rkey := l.c.key("lock", key)
	token := fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().Unix())

	ok, err := l.c.rdb.SetNX(ctx, rkey, token, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("redis: acquire lock %s: %w", key, err)
	}
	if !ok {
		return nil, false, nil
	}

	release := func(rctx context.Context) {
		if err := releaseScript.Run(rctx, l.c.rdb, []string{rkey}, token).Err(); err != nil {
			// Not fatal: the TTL releases it regardless. Worth logging because a
			// pattern of these means jobs are overrunning their lock.
			l.log.Warn("failed to release lock",
				slog.String("key", key), slog.String("error", err.Error()))
		}
	}
	return release, true, nil
}

// ---------------------------------------------------------------------------

// Cache implements port.Cache.
type Cache struct{ c *Client }

// NewCache returns a Cache.
func NewCache(c *Client) *Cache { return &Cache{c: c} }

var _ port.Cache = (*Cache)(nil)

// Set stores a value with a TTL.
func (ca *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if !ca.c.Available() {
		return nil
	}
	return ca.c.rdb.Set(ctx, ca.c.key("cache", key), value, ttl).Err()
}

// Get reads a value, returning port.ErrCacheMiss when absent.
func (ca *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	if !ca.c.Available() {
		return nil, port.ErrCacheMiss
	}
	b, err := ca.c.rdb.Get(ctx, ca.c.key("cache", key)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, port.ErrCacheMiss
	}
	return b, err
}

// Delete removes a value.
func (ca *Cache) Delete(ctx context.Context, key string) error {
	if !ca.c.Available() {
		return nil
	}
	return ca.c.rdb.Del(ctx, ca.c.key("cache", key)).Err()
}

// SetNX sets only if absent, reporting whether it did.
//
// This is the primitive behind replay protection: an AUTH nonce that fails to set
// has been seen before. The check-and-set must be atomic, or two concurrent
// replays of the same captured frame would both succeed.
//
// With Redis degraded it reports true — replay protection is unavailable, which is
// noted in the startup warning. The envelope's freshness window still bounds the
// exposure to MaxClockSkew.
func (ca *Cache) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	if !ca.c.Available() {
		return true, nil
	}
	return ca.c.rdb.SetNX(ctx, ca.c.key("cache", key), value, ttl).Result()
}
