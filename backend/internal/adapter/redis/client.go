// Package redis implements the ephemeral-infrastructure ports against Redis.
//
// Per PROJECT.md, Redis holds ONLY: the hot prompt-dispatch queue, presence,
// heartbeat, online devices, rate limiting, distributed locks, and temporary
// cache. Never permanent business data. Every value written here is either
// reconstructible from PostgreSQL or genuinely disposable, which is what allows
// the backend to keep serving when Redis is down.
package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
)

// Client wraps the Redis connection and implements lifecycle.Component.
type Client struct {
	rdb    *goredis.Client
	cfg    config.Redis
	log    *slog.Logger
	prefix string

	// degraded records that Redis was unreachable at startup while not required.
	// Read by Health so /health/ready can report "degraded" rather than "down".
	degraded bool
}

// New builds a Client. It does not connect; Start does.
func New(cfg config.Redis, log *slog.Logger) *Client {
	return &Client{
		cfg:    cfg,
		log:    log.With(slog.String("component", "redis")),
		prefix: cfg.KeyPrefix,
	}
}

// Name identifies the component in lifecycle logs.
func (c *Client) Name() string { return "redis" }

// Start connects and verifies reachability.
//
// When redis.required is false — the default — an unreachable Redis logs a warning
// and starts anyway. That is the deliberate consequence of ADR-0006: prompts are
// durable in PostgreSQL, so losing Redis costs latency rather than data, and
// refusing to boot would convert a recoverable degradation into an outage.
func (c *Client) Start(ctx context.Context) error {
	if c.cfg.URL == "" {
		if c.cfg.Required {
			return errors.New("redis: url is required but empty")
		}
		c.degraded = true
		c.log.Warn("no redis url configured; running degraded",
			slog.String("impact", "prompt dispatch falls back to reconnect delivery; rate limiting is not enforced"))
		return nil
	}

	opts, err := goredis.ParseURL(c.cfg.URL)
	if err != nil {
		return fmt.Errorf("redis: parse url: %w", err)
	}
	opts.PoolSize = c.cfg.PoolSize
	opts.DialTimeout = c.cfg.DialTimeout
	opts.ReadTimeout = c.cfg.ReadTimeout
	opts.WriteTimeout = c.cfg.WriteTimeout

	c.rdb = goredis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	if err := c.rdb.Ping(pingCtx).Err(); err != nil {
		if c.cfg.Required {
			_ = c.rdb.Close()
			c.rdb = nil
			return fmt.Errorf("redis: ping failed: %w", err)
		}
		c.degraded = true
		c.log.Warn("redis unreachable; running degraded",
			slog.String("error", err.Error()),
			slog.String("impact", "prompt dispatch falls back to reconnect delivery"))
		return nil
	}

	c.log.Info("connected", slog.String("key_prefix", c.prefix))
	return nil
}

// Stop closes the connection pool.
func (c *Client) Stop(context.Context) error {
	if c.rdb == nil {
		return nil
	}
	return c.rdb.Close()
}

// Available reports whether Redis can be used right now.
//
// Every caller checks this rather than assuming a client exists, because the
// degraded path is a supported operating mode, not an error state.
func (c *Client) Available() bool { return c.rdb != nil }

// Health reports connectivity for /health/ready.
//
// Returns nil when degraded-but-not-required: the backend genuinely still works,
// and reporting unhealthy would make a load balancer pull a serving instance out
// of rotation over a non-fatal condition.
func (c *Client) Health(ctx context.Context) error {
	if c.rdb == nil {
		if c.cfg.Required {
			return errors.New("redis: not connected")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c.rdb.Ping(ctx).Err()
}

// Degraded reports whether Redis is absent while not required.
func (c *Client) Degraded() bool { return c.degraded || c.rdb == nil }

// key namespaces a key with the configured prefix.
//
// Every key goes through here so a shared Upstash instance can host staging and
// production without either flushing the other's data.
func (c *Client) key(parts ...string) string {
	out := c.prefix
	for i, p := range parts {
		if i > 0 {
			out += ":"
		}
		out += p
	}
	return out
}

// Raw exposes the underlying client for the adapters in this package.
func (c *Client) Raw() *goredis.Client { return c.rdb }

// ---------------------------------------------------------------------------

// Presence implements port.PresenceTracker.
//
// Presence lives in Redis specifically because of TTL. A device that dies without
// a clean disconnect simply expires; a database column would stay "online" until
// something remembered to clear it, which is exactly the bug that makes a
// dashboard untrustworthy.
type Presence struct{ c *Client }

// NewPresence returns a Presence tracker.
func NewPresence(c *Client) *Presence { return &Presence{c: c} }

var _ port.PresenceTracker = (*Presence)(nil)

// MarkOnline records a device as connected, refreshed on every heartbeat.
//
// Two keys are written: a per-device key carrying the TTL, and a per-user set for
// listing. The set has no TTL of its own, so IsOnline is always checked per device
// rather than trusting set membership alone — a stale set entry must not make a
// dead device look alive.
func (p *Presence) MarkOnline(ctx context.Context, userID, deviceID string, ttl time.Duration) error {
	if !p.c.Available() {
		return nil // degraded: presence is best-effort
	}
	pipe := p.c.rdb.Pipeline()
	pipe.Set(ctx, p.c.key("presence", "device", deviceID), userID, ttl)
	pipe.SAdd(ctx, p.c.key("presence", "user", userID), deviceID)
	// The set is bounded by the user's device count, but expire it anyway so an
	// abandoned account does not leave keys behind forever.
	pipe.Expire(ctx, p.c.key("presence", "user", userID), 30*24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

// MarkOffline clears presence on a clean disconnect.
func (p *Presence) MarkOffline(ctx context.Context, userID, deviceID string) error {
	if !p.c.Available() {
		return nil
	}
	pipe := p.c.rdb.Pipeline()
	pipe.Del(ctx, p.c.key("presence", "device", deviceID))
	pipe.SRem(ctx, p.c.key("presence", "user", userID), deviceID)
	_, err := pipe.Exec(ctx)
	return err
}

// IsOnline reports device connectivity.
func (p *Presence) IsOnline(ctx context.Context, deviceID string) (bool, error) {
	if !p.c.Available() {
		return false, nil
	}
	n, err := p.c.rdb.Exists(ctx, p.c.key("presence", "device", deviceID)).Result()
	return n > 0, err
}

// OnlineForUser returns which of a user's devices are connected.
//
// Reads the set, then verifies each member's TTL key in one pipeline. The
// verification is the point: set membership alone would report devices that
// disconnected uncleanly as still online.
func (p *Presence) OnlineForUser(ctx context.Context, userID string) (map[string]bool, error) {
	out := map[string]bool{}
	if !p.c.Available() {
		return out, nil
	}

	members, err := p.c.rdb.SMembers(ctx, p.c.key("presence", "user", userID)).Result()
	if err != nil || len(members) == 0 {
		return out, err
	}

	pipe := p.c.rdb.Pipeline()
	cmds := make(map[string]*goredis.IntCmd, len(members))
	for _, deviceID := range members {
		cmds[deviceID] = pipe.Exists(ctx, p.c.key("presence", "device", deviceID))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
		return out, err
	}

	var stale []string
	for deviceID, cmd := range cmds {
		if n, err := cmd.Result(); err == nil && n > 0 {
			out[deviceID] = true
		} else {
			stale = append(stale, deviceID)
		}
	}

	// Opportunistically prune expired members so the set does not grow without
	// bound across a device's lifetime of reconnects.
	if len(stale) > 0 {
		_ = p.c.rdb.SRem(ctx, p.c.key("presence", "user", userID), stale).Err()
	}
	return out, nil
}
