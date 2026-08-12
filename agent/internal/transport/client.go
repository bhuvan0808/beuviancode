package transport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bhuvan0808/beuviancode/agent/internal/config"
	"github.com/bhuvan0808/beuviancode/agent/internal/store"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
	"github.com/bhuvan0808/beuviancode/shared/retry"
	"github.com/bhuvan0808/beuviancode/shared/version"
)

// Handler receives inbound frames from the backend.
//
// Implemented by the session manager. An interface rather than a callback field so
// the transport's dependency is visible in its constructor.
type Handler interface {
	// HandleFrame processes one inbound envelope. An error is logged; it never
	// tears down the connection, because one malformed frame must not cost the
	// user their session.
	HandleFrame(ctx context.Context, env protocol.Envelope) error

	// OnConnected fires after a successful handshake, so the session manager can
	// resend current status and flush anything queued while offline.
	OnConnected(ctx context.Context)

	// OnDisconnected fires when the connection drops.
	OnDisconnected(ctx context.Context, err error)
}

// Client maintains the authenticated WebSocket to the backend.
//
// Reconnection is the normal case, not an error path. The agent runs on a laptop
// that sleeps, changes networks, and gets closed mid-session; a transport that
// treated a disconnect as fatal would be useless.
type Client struct {
	cfg     config.Backend
	store   *store.Store
	handler Handler
	log     *slog.Logger

	mu   sync.Mutex
	conn *websocket.Conn

	// outbound buffers frames while disconnected. Bounded: a long outage must not
	// grow memory without limit.
	outbound chan []byte

	// connected is closed and replaced on each connection, so callers can wait
	// for a live connection without polling.
	connectedMu sync.RWMutex
	connected   bool

	// seq is the per-connection monotonic counter the protocol uses to let the
	// receiver detect gaps.
	seq uint64

	// seenInbound deduplicates redelivered frames by envelope ID. The backend may
	// redeliver a PROMPT it never saw acknowledged.
	seenMu   sync.Mutex
	seen     map[string]time.Time
	capsFn   func() []string
	stopOnce sync.Once
	stopped  chan struct{}
}

// Deps groups the client's collaborators.
type Deps struct {
	Config  config.Backend
	Store   *store.Store
	Handler Handler
	Log     *slog.Logger

	// Capabilities reports which coding agents are installed, evaluated at each
	// handshake rather than once at startup: a user may install Claude Code while
	// the agent is running, and the backend needs to know.
	Capabilities func() []string
}

// New builds a Client.
func New(d Deps) *Client {
	return &Client{
		cfg:     d.Config,
		store:   d.Store,
		handler: d.Handler,
		log:     d.Log.With(slog.String("component", "transport")),
		// Sized so a burst of log frames during a disconnect survives until the
		// connection returns.
		outbound: make(chan []byte, 2048),
		seen:     make(map[string]time.Time),
		capsFn:   d.Capabilities,
		stopped:  make(chan struct{}),
	}
}

// SetHandler installs the frame handler.
//
// The transport and the session manager reference each other: the manager needs a
// Sender, and the transport needs a Handler. Rather than resolve that with an
// indirection both sides pay for at every call, the cycle is broken once at
// construction — build the client, build the manager with it, then complete the
// wiring here. Must be called before Start.
func (c *Client) SetHandler(h Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = h
}

// Name identifies the component in lifecycle logs.
func (c *Client) Name() string { return "transport" }

// Start begins the connection loop. Non-blocking, as lifecycle.Component requires.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	handler := c.handler
	c.mu.Unlock()
	if handler == nil {
		// A wiring mistake, not a runtime condition. Failing at startup is far
		// better than silently discarding every inbound prompt.
		return errors.New("transport: no frame handler was installed")
	}

	state := c.store.Current()
	if !state.Registered() {
		return errors.New("transport: device is not registered")
	}
	go c.run(context.WithoutCancel(ctx))
	return nil
}

// currentHandler returns the installed handler.
func (c *Client) currentHandler() Handler {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handler
}

// Stop closes the connection.
func (c *Client) Stop(context.Context) error {
	c.stopOnce.Do(func() { close(c.stopped) })
	c.closeConn(websocket.CloseNormalClosure, "agent shutting down")
	return nil
}

// run is the reconnect loop.
//
// Deliberately unbounded: a laptop closed for the weekend must reconnect on
// Monday, not have given up on Friday. The one thing that must NOT loop forever is
// a rejected credential — an agent with a revoked token retrying every 30 seconds
// becomes a denial-of-service against our own gateway, multiplied by every
// installed agent. connect returns a permanent error for those.
func (c *Client) run(ctx context.Context) {
	backoff := retry.New(retry.ReconnectPolicy())

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopped:
			return
		default:
		}

		err := c.connect(ctx)

		switch {
		case err == nil:
			// Clean disconnect. Reset so a later blip does not inherit a long
			// delay from this session's history.
			backoff.Reset()

		case isPermanent(err):
			// Credentials are wrong or the protocol version is unsupported.
			// Retrying cannot help and would only generate load.
			c.log.Error("connection permanently rejected; not retrying",
				blog.Err(err),
				slog.String("action", "re-register the device with `beuvian-agent -register`"))
			return

		default:
			c.log.Warn("connection lost; will retry",
				blog.Err(err), slog.Int("attempt", backoff.Attempt()+1))
		}

		// Jitter here is load-bearing, not decoration: when the backend restarts,
		// every connected agent reconnects at once. Without jitter their retries
		// stay synchronised and the herd keeps the gateway down.
		if werr := backoff.Wait(ctx); werr != nil {
			return
		}
	}
}

// permanentError marks a failure that must stop the reconnect loop.
type permanentError struct{ err error }

func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

func isPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// connect performs one connection attempt and serves it until it drops.
func (c *Client) connect(ctx context.Context) error {
	state := c.store.Current()
	if !state.Registered() {
		return &permanentError{errors.New("device is not registered")}
	}

	endpoint, err := url.Parse(c.cfg.URL)
	if err != nil {
		return &permanentError{fmt.Errorf("invalid backend url: %w", err)}
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: c.cfg.ConnectTimeout,
		// Buffers matched to the protocol's expectations; the default 4 KB is fine
		// for control frames but wasteful to re-allocate for log batches.
		ReadBufferSize:  8192,
		WriteBufferSize: 8192,
	}
	if c.cfg.InsecureSkipVerify {
		// Configuration validation refuses this for wss:// endpoints, so reaching
		// here means a deliberate ws:// local test.
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // guarded by config validation
	}

	header := http.Header{}
	header.Set("User-Agent", version.UserAgent("agent"))

	conn, resp, err := dialer.DialContext(ctx, endpoint.String(), header)
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			// A 401 or 403 at the HTTP layer is a credential problem, not a
			// network one, so it must not be retried forever.
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return &permanentError{fmt.Errorf("backend rejected the connection: %s", resp.Status)}
			}
		}
		return fmt.Errorf("dial %s: %w", endpoint.Redacted(), err)
	}
	conn.SetReadLimit(protocol.MaxMessageBytes)

	c.mu.Lock()
	c.conn = conn
	c.seq = 0 // the sequence is per-connection
	c.mu.Unlock()

	defer func() {
		c.setConnected(false)
		_ = conn.Close()
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		c.currentHandler().OnDisconnected(ctx, err)
	}()

	if err := c.authenticate(ctx, conn, state); err != nil {
		return err
	}

	c.setConnected(true)
	c.log.Info("connected", slog.String("backend", endpoint.Redacted()))
	c.currentHandler().OnConnected(ctx)

	return c.serve(ctx, conn)
}

// authenticate performs the AUTH handshake and waits for the ACK.
func (c *Client) authenticate(ctx context.Context, conn *websocket.Conn, state store.State) error {
	hostname, _ := os.Hostname()

	env, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypeAuth, time.Now().UTC(),
		protocol.AuthPayload{
			Token:    state.DeviceToken,
			DeviceID: state.DeviceID,
			// A FRESH nonce every connection. Reusing one is rejected as a replay,
			// which would make every reconnect after the first fail.
			Nonce:        id.Nonce(),
			AgentVersion: version.Get().Version,
			Platform:     runtime.GOOS + "/" + runtime.GOARCH,
			Hostname:     hostname,
			Capabilities: c.capabilities(),
		})
	if err != nil {
		return err
	}
	env.DeviceID = state.DeviceID

	if err := conn.SetWriteDeadline(time.Now().Add(c.cfg.ConnectTimeout)); err != nil {
		return err
	}
	if err := conn.WriteJSON(env); err != nil {
		return fmt.Errorf("send AUTH: %w", err)
	}

	// The connection is unauthenticated until ACK arrives, so a deadline here
	// bounds how long a silent backend can hold us.
	if err := conn.SetReadDeadline(time.Now().Add(c.cfg.ConnectTimeout)); err != nil {
		return err
	}
	var reply protocol.Envelope
	if err := conn.ReadJSON(&reply); err != nil {
		return fmt.Errorf("read AUTH reply: %w", err)
	}

	switch reply.Type {
	case protocol.TypeAck:
		payload, derr := protocol.Decode[protocol.AckPayload](reply)
		if derr != nil {
			return derr
		}
		if !payload.Accepted {
			return &permanentError{fmt.Errorf("authentication rejected: %s", payload.Reason)}
		}
		return nil

	case protocol.TypeError:
		payload, derr := protocol.Decode[protocol.ErrorPayload](reply)
		if derr != nil {
			return derr
		}
		err := fmt.Errorf("backend error %s: %s", payload.Code, payload.Message)
		// The backend tells us whether retrying can help. Honouring it is what
		// stops a revoked agent hammering the gateway forever.
		if !payload.Retryable {
			// A revoked or unknown device will never recover with these
			// credentials; clearing them forces a clean re-registration rather
			// than an endless rejected-reconnect loop.
			if payload.Code == protocol.ErrCodeDeviceNotFound || payload.Code == protocol.ErrCodeUnauthorized {
				if rerr := c.store.Reset(); rerr != nil {
					c.log.Error("failed to clear rejected credentials", blog.Err(rerr))
				}
			}
			return &permanentError{err}
		}
		return err

	default:
		return fmt.Errorf("unexpected reply to AUTH: %s", reply.Type)
	}
}

// serve runs the read, write, and heartbeat pumps until the connection ends.
func (c *Client) serve(ctx context.Context, conn *websocket.Conn) error {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, 3)

	go func() { errs <- c.readPump(connCtx, conn) }()
	go func() { errs <- c.writePump(connCtx, conn) }()
	go func() { errs <- c.heartbeat(connCtx, conn) }()

	select {
	case err := <-errs:
		return err
	case <-c.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readPump consumes inbound frames.
func (c *Client) readPump(ctx context.Context, conn *websocket.Conn) error {
	// A read deadline is what detects a peer that vanished without closing — a
	// closed laptop lid looks exactly like a healthy idle connection otherwise.
	// Each frame extends it.
	if err := conn.SetReadDeadline(time.Now().Add(protocol.HeartbeatTimeout)); err != nil {
		return err
	}

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(protocol.HeartbeatTimeout))

		if err := env.Validate(); err != nil {
			c.log.Warn("discarding invalid frame", blog.Err(err))
			continue
		}

		// Deduplicate: the backend may redeliver a PROMPT it never saw
		// acknowledged, and injecting the same instruction twice would make the
		// coding agent redo work.
		if c.alreadySeen(env.ID) {
			c.log.Debug("ignoring duplicate frame", slog.String("id", env.ID))
			continue
		}

		if env.Type == protocol.TypePong {
			continue // liveness only; the deadline was already extended
		}
		if env.Type == protocol.TypePing {
			c.replyPong(env)
			continue
		}

		if err := c.currentHandler().HandleFrame(ctx, env); err != nil {
			// Logged, never fatal: one bad frame must not cost the user their
			// session.
			c.log.Warn("frame handling failed",
				slog.String("type", env.Type.String()), blog.Err(err))
		}
	}
}

// writePump drains the outbound queue onto the socket.
//
// The single owner of the write side. A websocket permits one concurrent writer,
// and funnelling everything through here makes that safe without a mutex on every
// send.
func (c *Client) writePump(ctx context.Context, conn *websocket.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopped:
			return nil
		case payload := <-c.outbound:
			if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return err
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				// Put it back so it survives the reconnect rather than being lost.
				c.requeue(payload)
				return fmt.Errorf("write: %w", err)
			}
		}
	}
}

// heartbeat sends PING every interval.
func (c *Client) heartbeat(ctx context.Context, conn *websocket.Conn) error {
	ticker := time.NewTicker(protocol.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopped:
			return nil
		case <-ticker.C:
			env, err := protocol.NewEnvelope(
				id.WithPrefix(id.PrefixMessage), protocol.TypePing, time.Now().UTC(),
				protocol.HeartbeatPayload{Nonce: id.Nonce(), SentAt: time.Now().UTC()})
			if err != nil {
				continue
			}
			if err := c.Send(env); err != nil {
				return err
			}
		}
	}
}

// replyPong answers a server-initiated PING.
func (c *Client) replyPong(ping protocol.Envelope) {
	payload, err := protocol.Decode[protocol.HeartbeatPayload](ping)
	if err != nil {
		payload = protocol.HeartbeatPayload{}
	}
	env, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypePong, time.Now().UTC(),
		// Echo the nonce unchanged so the peer can match the reply to its probe.
		protocol.HeartbeatPayload{Nonce: payload.Nonce, SentAt: time.Now().UTC()})
	if err == nil {
		_ = c.Send(env)
	}
}

// Send queues an envelope for delivery.
//
// Never blocks and never fails when disconnected: frames buffer until the
// connection returns. That is what lets the session manager keep reporting status
// through an outage without special-casing connectivity everywhere.
func (c *Client) Send(env protocol.Envelope) error {
	state := c.store.Current()
	if env.DeviceID == "" {
		env.DeviceID = state.DeviceID
	}

	c.mu.Lock()
	c.seq++
	env.Sequence = c.seq
	c.mu.Unlock()

	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("transport: encode %s: %w", env.Type, err)
	}

	select {
	case c.outbound <- payload:
		return nil
	default:
		// The buffer is full after a long outage. Drop the OLDEST frame rather
		// than the newest: current status matters more than a stale one, and the
		// session manager marks log batches truncated so the gap is visible.
		select {
		case <-c.outbound:
			c.log.Warn("outbound buffer full; dropped the oldest frame")
		default:
		}
		select {
		case c.outbound <- payload:
			return nil
		default:
			return errors.New("transport: outbound buffer is full")
		}
	}
}

// requeue puts a frame back at the front after a failed write.
func (c *Client) requeue(payload []byte) {
	select {
	case c.outbound <- payload:
	default:
		// Full. Losing one frame is preferable to blocking the write pump, which
		// would stall the reconnect.
	}
}

// Connected reports whether a live authenticated connection exists.
func (c *Client) Connected() bool {
	c.connectedMu.RLock()
	defer c.connectedMu.RUnlock()
	return c.connected
}

func (c *Client) setConnected(v bool) {
	c.connectedMu.Lock()
	c.connected = v
	c.connectedMu.Unlock()
}

// QueueDepth reports how many frames are buffered, for status reporting.
func (c *Client) QueueDepth() int { return len(c.outbound) }

// capabilities reports installed coding agents, guarding against a nil provider.
func (c *Client) capabilities() []string {
	if c.capsFn == nil {
		return []string{}
	}
	caps := c.capsFn()
	if caps == nil {
		return []string{}
	}
	return caps
}

// alreadySeen reports whether this envelope ID was processed recently.
//
// The window is bounded and pruned, because an unbounded set would grow for the
// life of the process — a memory leak that only shows up after days of uptime,
// which is exactly how long these agents run.
func (c *Client) alreadySeen(envelopeID string) bool {
	const window = 10 * time.Minute

	c.seenMu.Lock()
	defer c.seenMu.Unlock()

	now := time.Now()
	if _, ok := c.seen[envelopeID]; ok {
		return true
	}

	// Amortised pruning: only sweep when the map has grown enough to matter.
	if len(c.seen) > 2048 {
		for k, at := range c.seen {
			if now.Sub(at) > window {
				delete(c.seen, k)
			}
		}
	}
	c.seen[envelopeID] = now
	return false
}

// closeConn sends a close frame and terminates the connection.
func (c *Client) closeConn(code int, reason string) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	// Best-effort close frame so the backend records a graceful disconnect and the
	// dashboard says "went offline" rather than "lost connection".
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = conn.Close()
}
