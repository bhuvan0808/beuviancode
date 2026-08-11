package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"

	"github.com/bhuvan0808/beuviancode/backend/internal/app"
	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// Gateway serves the WebSocket endpoint.
type Gateway struct {
	hub      *Hub
	cfg      config.WebSocket
	auth     *app.AuthService
	devices  *app.DeviceService
	sessions *app.SessionService
	prompts  *app.PromptService
	notifs   *app.NotificationService
	cache    port.Cache
	events   port.EventPublisher
	clock    port.Clock
	log      *slog.Logger
}

// GatewayDeps groups the gateway's collaborators.
type GatewayDeps struct {
	Hub      *Hub
	Config   config.WebSocket
	Auth     *app.AuthService
	Devices  *app.DeviceService
	Sessions *app.SessionService
	Prompts  *app.PromptService
	Notifs   *app.NotificationService
	Cache    port.Cache
	Events   port.EventPublisher
	Clock    port.Clock
	Log      *slog.Logger
}

// NewGateway builds a Gateway.
func NewGateway(d GatewayDeps) *Gateway {
	return &Gateway{
		hub: d.Hub, cfg: d.Config, auth: d.Auth, devices: d.Devices,
		sessions: d.Sessions, prompts: d.Prompts, notifs: d.Notifs,
		cache: d.Cache, events: d.Events, clock: d.Clock,
		log: d.Log.With(slog.String("component", "ws")),
	}
}

// Handler returns the Fiber handler for /v1/ws.
//
// Two-stage: an HTTP middleware that rejects non-upgrade requests, then the
// websocket handler itself. Fiber's contrib package requires this shape.
func (g *Gateway) Handler() fiber.Handler {
	upgrade := func(c *fiber.Ctx) error {
		if !websocket.IsWebSocketUpgrade(c) {
			return fiber.ErrUpgradeRequired
		}
		// Carry the request's correlation ID into the connection, so frames on
		// this socket can be traced back to the handshake that opened it.
		c.Locals("correlation_id", blog.CorrelationIDFrom(c.UserContext()))
		return c.Next()
	}

	return func(c *fiber.Ctx) error {
		if err := upgrade(c); err != nil {
			return err
		}
		return websocket.New(g.serve, websocket.Config{
			ReadBufferSize:  g.cfg.ReadBufferSize,
			WriteBufferSize: g.cfg.WriteBufferSize,
		})(c)
	}
}

// serve runs one connection's lifecycle.
func (g *Gateway) serve(ws *websocket.Conn) {
	connID := id.WithPrefix("con")
	log := g.log.With(slog.String("conn_id", connID))

	// Frames larger than the protocol limit are rejected by the library before
	// reaching us, so a hostile peer cannot exhaust memory with one huge message.
	ws.SetReadLimit(protocol.MaxMessageBytes)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The connection is unauthenticated until AUTH is answered with ACK. A
	// deadline on the first read bounds how long an unauthenticated socket can
	// hold gateway resources.
	if err := ws.SetReadDeadline(g.clock.Now().Add(g.cfg.HandshakeTimeout)); err != nil {
		log.Warn("failed to set handshake deadline", blog.Err(err))
		return
	}

	conn, session, err := g.authenticate(ctx, ws, connID, log)
	if err != nil {
		g.writeError(ws, "", err)
		log.Info("handshake rejected", blog.Err(err))
		return
	}
	log = log.With(
		slog.String(blog.FieldUserID, conn.UserID),
		slog.String(blog.FieldDeviceID, conn.DeviceID))

	g.hub.Register(conn)
	defer func() {
		g.hub.Unregister(conn)
		conn.Close()
		g.onDisconnect(context.WithoutCancel(ctx), conn, log)
	}()

	// The writer owns the socket exclusively. Every other goroutine queues through
	// conn.Send, which is what makes concurrent sends safe without a write mutex.
	go g.writePump(ws, conn, log)

	if conn.IsDevice() {
		g.onDeviceConnected(ctx, conn, session, log)
	}

	g.readPump(ctx, ws, conn, log)
}

// authenticate performs the AUTH handshake.
//
// Devices authenticate with an AUTH frame carrying a device token. Dashboard
// clients authenticate with a query-parameter access token, because a browser
// cannot set an Authorization header on a WebSocket handshake — a genuine
// limitation of the browser API, not a shortcut.
func (g *Gateway) authenticate(ctx context.Context, ws *websocket.Conn, connID string, log *slog.Logger) (*Conn, domain.Session, error) {
	// Dashboard path: token in the query string, validated before the first frame.
	if token := ws.Query("access_token"); token != "" {
		claims, err := g.auth.VerifyAccess(token)
		if err != nil {
			return nil, domain.Session{}, err
		}
		return g.newConn(connID, claims.Subject, ""), domain.Session{}, nil
	}

	// Device path: the first frame must be AUTH.
	_, raw, err := ws.ReadMessage()
	if err != nil {
		return nil, domain.Session{}, fmt.Errorf("%w: no auth frame", domain.ErrUnauthorized)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, domain.Session{}, fmt.Errorf("%w: malformed auth frame", domain.ErrValidation)
	}
	if err := env.Validate(); err != nil {
		return nil, domain.Session{}, err
	}
	if env.Type != protocol.TypeAuth {
		// Anything before AUTH ends the connection, which bounds the pre-auth
		// attack surface to exactly one message type.
		return nil, domain.Session{}, fmt.Errorf("%w: expected AUTH first", domain.ErrUnauthorized)
	}

	payload, err := protocol.Decode[protocol.AuthPayload](env)
	if err != nil {
		return nil, domain.Session{}, err
	}

	// Replay protection, part one: the frame must be recent.
	if !env.FreshWithin(g.clock.Now(), protocol.MaxClockSkew) {
		return nil, domain.Session{}, fmt.Errorf("%w: stale auth frame", domain.ErrUnauthorized)
	}
	// Part two: the nonce must be unseen. SetNX is atomic, so two concurrent
	// replays of a captured frame cannot both succeed.
	if payload.Nonce == "" {
		return nil, domain.Session{}, fmt.Errorf("%w: missing nonce", domain.ErrUnauthorized)
	}
	fresh, err := g.cache.SetNX(ctx, "authnonce:"+payload.Nonce, []byte("1"), protocol.MaxClockSkew*2)
	if err == nil && !fresh {
		return nil, domain.Session{}, fmt.Errorf("%w: nonce replay", domain.ErrUnauthorized)
	}

	device, err := g.devices.AuthenticateDevice(ctx, payload.Token, payload.DeviceID)
	if err != nil {
		return nil, domain.Session{}, err
	}

	// Record what the agent reports as installed, so prompt dispatch can avoid
	// devices that cannot service a given adapter.
	if len(payload.Capabilities) > 0 {
		device.Capabilities = payload.Capabilities
		device.AgentVersion = payload.AgentVersion
		if err := g.devices.UpdateFromHandshake(ctx, device); err != nil {
			log.Warn("failed to record device capabilities", blog.Err(err))
		}
	}

	conn := g.newConn(connID, device.UserID, device.ID)

	// ACK, not a dedicated AUTH_OK: the protocol's 13-type set is closed, and ACK
	// is the universal positive reply.
	g.writeAck(ws, env.ID, true, "")

	// Clear the handshake deadline; the heartbeat governs liveness from here.
	_ = ws.SetReadDeadline(time.Time{})

	active, _ := g.sessions.ActiveForDevice(ctx, device.ID)
	return conn, active, nil
}

func (g *Gateway) newConn(connID, userID, deviceID string) *Conn {
	return &Conn{
		ID:       connID,
		UserID:   userID,
		DeviceID: deviceID,
		send:     make(chan []byte, g.cfg.SendQueueSize),
		closed:   make(chan struct{}),
	}
}

// onDeviceConnected records presence and flushes queued prompts.
func (g *Gateway) onDeviceConnected(ctx context.Context, conn *Conn, session domain.Session, log *slog.Logger) {
	device := domain.Device{ID: conn.DeviceID, UserID: conn.UserID}
	if err := g.devices.MarkOnline(ctx, device); err != nil {
		log.Warn("failed to mark device online", blog.Err(err))
	}

	// Tell the user's dashboards, so a device appearing is visible immediately
	// rather than at the next poll.
	g.publishPresence(ctx, conn, protocol.TypeDeviceOnline, true)

	// Flush anything queued while the device was away. This is the path that makes
	// Redis disposable: prompts sent to an offline laptop arrive here.
	if n, err := g.prompts.DeliverPending(ctx, conn.DeviceID); err != nil {
		log.Warn("failed to flush queued prompts", blog.Err(err))
	} else if n > 0 {
		log.Info("delivered queued prompts", slog.Int("count", n))
	}

	if session.ID != "" {
		log.Info("device reconnected to active session", slog.String("session_id", session.ID))
	}
}

// onDisconnect clears presence and notifies dashboards.
func (g *Gateway) onDisconnect(ctx context.Context, conn *Conn, log *slog.Logger) {
	if !conn.IsDevice() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := g.devices.MarkOffline(ctx, conn.UserID, conn.DeviceID); err != nil {
		log.Warn("failed to clear presence", blog.Err(err))
	}
	g.publishPresence(ctx, conn, protocol.TypeDeviceOffline, false)
	log.Info("device disconnected")
}

// publishPresence notifies dashboards of a device coming or going.
func (g *Gateway) publishPresence(ctx context.Context, conn *Conn, msgType protocol.MessageType, graceful bool) {
	env, err := protocol.NewEnvelope(id.WithPrefix(id.PrefixMessage), msgType, g.clock.Now(),
		protocol.DevicePresencePayload{
			DeviceID: conn.DeviceID,
			At:       g.clock.Now(),
			Graceful: graceful,
		})
	if err != nil {
		return
	}
	env.DeviceID = conn.DeviceID
	// Through the publisher, not the local hub: the user's dashboard may be
	// connected to a different backend instance.
	if err := g.events.PublishToUser(ctx, conn.UserID, env); err != nil {
		g.log.Debug("failed to publish presence", blog.Err(err))
	}
}

// readPump consumes inbound frames until the connection ends.
func (g *Gateway) readPump(ctx context.Context, ws *websocket.Conn, conn *Conn, log *slog.Logger) {
	// A read deadline is what detects a peer that vanished without closing —
	// a laptop lid closing mid-session looks exactly like a healthy idle
	// connection otherwise. Each frame extends it.
	_ = ws.SetReadDeadline(g.clock.Now().Add(protocol.HeartbeatTimeout))

	for {
		select {
		case <-conn.Closed():
			return
		case <-ctx.Done():
			return
		default:
		}

		_, raw, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Info("connection closed unexpectedly", blog.Err(err))
			}
			return
		}
		_ = ws.SetReadDeadline(g.clock.Now().Add(protocol.HeartbeatTimeout))

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			g.writeError(ws, "", fmt.Errorf("%w: malformed frame", domain.ErrValidation))
			continue
		}
		if err := env.Validate(); err != nil {
			// An unsupported protocol version is terminal: the agent must be
			// upgraded, and retrying would only generate load.
			if errors.Is(err, protocol.ErrUnsupportedVersion) {
				g.writeError(ws, env.ID, err)
				return
			}
			g.writeError(ws, env.ID, err)
			continue
		}

		if err := g.dispatch(ctx, conn, env, log); err != nil {
			log.Warn("frame handling failed",
				slog.String("type", env.Type.String()), blog.Err(err))
			g.writeError(ws, env.ID, err)
		}
	}
}

// writePump drains the outbound queue.
//
// The single owner of the socket's write side. A websocket connection permits one
// concurrent writer, and funnelling every send through here is what makes that
// safe without a mutex on every write.
func (g *Gateway) writePump(ws *websocket.Conn, conn *Conn, log *slog.Logger) {
	defer conn.Close()

	for {
		select {
		case <-conn.Closed():
			// Best-effort close frame so the peer learns this was deliberate and
			// can distinguish it from a network failure.
			_ = ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(time.Second))
			return

		case payload, ok := <-conn.Outbound():
			if !ok {
				return
			}
			// A write deadline stops a stalled peer holding this goroutine and the
			// connection's buffers indefinitely.
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ws.WriteMessage(websocket.TextMessage, payload); err != nil {
				log.Debug("write failed; closing connection", blog.Err(err))
				return
			}
		}
	}
}
