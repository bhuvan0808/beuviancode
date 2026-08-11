// Package ws implements the WebSocket gateway and connection hub.
package ws

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// Conn is one live WebSocket connection.
//
// Writes go through a buffered channel rather than directly to the socket, for a
// specific reason: a websocket connection supports exactly one concurrent writer,
// and several goroutines (the prompt dispatcher, the event fan-out, the heartbeat)
// all need to send. A single writer goroutine draining this channel makes that
// safe without a mutex around every write.
type Conn struct {
	ID       string
	UserID   string
	DeviceID string // empty for dashboard connections

	send   chan []byte
	closed chan struct{}

	// once guards Close so a connection torn down from several goroutines at once
	// closes its channels exactly once.
	once sync.Once
}

// IsDevice reports whether this is an agent rather than a dashboard connection.
func (c *Conn) IsDevice() bool { return c.DeviceID != "" }

// Send queues an encoded frame.
//
// Returns false when the outbound buffer is full, which the caller treats as a
// dead connection. Dropping a slow consumer is deliberate: blocking here would let
// one stalled phone on a train freeze the broadcaster for every other client.
func (c *Conn) Send(payload []byte) bool {
	select {
	case c.send <- payload:
		return true
	case <-c.closed:
		return false
	default:
		// Buffer full. The caller closes the connection; the client reconnects and
		// resynchronises, which is cheaper than stalling the whole gateway.
		return false
	}
}

// Close terminates the connection. Safe to call repeatedly.
func (c *Conn) Close() {
	c.once.Do(func() { close(c.closed) })
}

// Closed returns a channel that is closed when the connection ends.
func (c *Conn) Closed() <-chan struct{} { return c.closed }

// Outbound returns the write queue, drained by the connection's writer goroutine.
func (c *Conn) Outbound() <-chan []byte { return c.send }

// Hub tracks the connections owned by THIS backend instance.
//
// Instance-local by design. Cross-instance delivery goes through Redis pub/sub
// (port.EventPublisher and port.PromptDispatcher), which keeps the hot path — a
// write to a socket this process already owns — a plain map lookup with no network
// round trip.
type Hub struct {
	mu sync.RWMutex

	// byDevice maps device ID to its single connection. A device may hold only one:
	// a second AUTH for the same device replaces the first, which is what makes a
	// reconnect after an unclean disconnect work rather than accumulating zombies.
	byDevice map[string]*Conn

	// byUser maps user ID to every connection they own, agent and dashboard alike.
	// A user legitimately has several: two laptops and three browser tabs.
	byUser map[string]map[string]*Conn

	log *slog.Logger
}

// NewHub returns an empty Hub.
func NewHub(log *slog.Logger) *Hub {
	return &Hub{
		byDevice: make(map[string]*Conn),
		byUser:   make(map[string]map[string]*Conn),
		log:      log.With(slog.String("component", "hub")),
	}
}

var _ port.ConnectionRegistry = (*Hub)(nil)

// Register adds a connection, evicting any prior connection for the same device.
func (h *Hub) Register(c *Conn) {
	h.mu.Lock()
	var evicted *Conn
	if c.IsDevice() {
		if existing, ok := h.byDevice[c.DeviceID]; ok && existing.ID != c.ID {
			evicted = existing
			delete(h.byUser[existing.UserID], existing.ID)
		}
		h.byDevice[c.DeviceID] = c
	}
	if h.byUser[c.UserID] == nil {
		h.byUser[c.UserID] = make(map[string]*Conn)
	}
	h.byUser[c.UserID][c.ID] = c
	h.mu.Unlock()

	// Close the evicted connection outside the lock: Close can block on channel
	// operations, and holding the hub lock while it does would stall every other
	// connection in the process.
	if evicted != nil {
		h.log.Info("replacing existing device connection",
			slog.String("device_id", c.DeviceID))
		evicted.Close()
	}
}

// Unregister removes a connection.
func (h *Hub) Unregister(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Only remove the device mapping if it still points at THIS connection. After
	// an eviction the map already holds the replacement, and deleting blindly
	// would unregister the live connection.
	if c.IsDevice() {
		if current, ok := h.byDevice[c.DeviceID]; ok && current.ID == c.ID {
			delete(h.byDevice, c.DeviceID)
		}
	}
	if conns, ok := h.byUser[c.UserID]; ok {
		delete(conns, c.ID)
		if len(conns) == 0 {
			delete(h.byUser, c.UserID)
		}
	}
}

// SendToDevice writes an envelope to a device connected to this instance.
func (h *Hub) SendToDevice(deviceID string, env protocol.Envelope) bool {
	h.mu.RLock()
	c, ok := h.byDevice[deviceID]
	h.mu.RUnlock()
	if !ok {
		return false
	}

	payload, err := json.Marshal(env)
	if err != nil {
		h.log.Error("failed to encode envelope",
			slog.String("type", env.Type.String()), slog.String("error", err.Error()))
		return false
	}
	if !c.Send(payload) {
		// A full buffer means the peer stopped reading. Drop it rather than
		// letting it accumulate memory; the agent reconnects and resynchronises.
		h.log.Warn("device send buffer full; closing connection",
			slog.String("device_id", deviceID))
		c.Close()
		return false
	}
	return true
}

// SendToUser writes an envelope to every dashboard connection a user has here.
//
// Deliberately excludes device connections. A dashboard event echoed back to the
// agent would be noise at best, and at worst a feedback loop where a status update
// triggers another status update.
func (h *Hub) SendToUser(userID string, env protocol.Envelope) int {
	h.mu.RLock()
	conns := make([]*Conn, 0, len(h.byUser[userID]))
	for _, c := range h.byUser[userID] {
		if !c.IsDevice() {
			conns = append(conns, c)
		}
	}
	h.mu.RUnlock()

	if len(conns) == 0 {
		return 0
	}

	// Encode once for all recipients: a user with several tabs open should not pay
	// for repeated marshalling of the same frame.
	payload, err := json.Marshal(env)
	if err != nil {
		h.log.Error("failed to encode envelope", slog.String("error", err.Error()))
		return 0
	}

	sent := 0
	for _, c := range conns {
		if c.Send(payload) {
			sent++
		} else {
			c.Close()
		}
	}
	return sent
}

// DeviceConnected reports whether a device is connected to this instance.
func (h *Hub) DeviceConnected(deviceID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.byDevice[deviceID]
	return ok
}

// CloseDevice terminates a device's connection, used on revocation.
func (h *Hub) CloseDevice(deviceID, reason string) {
	h.mu.RLock()
	c, ok := h.byDevice[deviceID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	h.log.Info("closing device connection",
		slog.String("device_id", deviceID), slog.String("reason", reason))
	c.Close()
}

// Count returns the number of live connections, for health reporting.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, conns := range h.byUser {
		n += len(conns)
	}
	return n
}

// ConnectedDevices lists devices connected to this instance.
//
// Used by the prompt reconciliation sweep, which only has work to do for devices
// it can actually write to. Not part of the ConnectionRegistry port: nothing on the
// hot path needs to enumerate connections, and exposing it there would invite
// callers to iterate rather than address a connection directly.
func (h *Hub) ConnectedDevices() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.byDevice))
	for deviceID := range h.byDevice {
		out = append(out, deviceID)
	}
	return out
}

// CloseAll terminates every connection, used during shutdown.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	conns := make([]*Conn, 0, 16)
	for _, byID := range h.byUser {
		for _, c := range byID {
			conns = append(conns, c)
		}
	}
	h.byDevice = make(map[string]*Conn)
	h.byUser = make(map[string]map[string]*Conn)
	h.mu.Unlock()

	for _, c := range conns {
		c.Close()
	}
}
