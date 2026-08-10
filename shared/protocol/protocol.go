// Package protocol defines the versioned wire format shared by the Beuvian
// Desktop Agent and the Beuvian backend WebSocket gateway.
//
// This package is the single authoritative definition of the protocol. Both
// sides of the connection compile against these exact types, which makes a
// silent producer/consumer drift impossible: a change to a payload struct breaks
// the build on both ends rather than failing at runtime in production.
//
// The human-readable specification lives in docs/WEBSOCKET_PROTOCOL.md and MUST
// be updated in the same commit as any change here.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Version is the current protocol version.
//
// Versioning contract:
//   - Additive changes (a new optional payload field, a new MessageType that
//     older peers may ignore) do NOT bump Version.
//   - Removing or repurposing a field, or changing a field's meaning, DOES bump
//     Version.
//
// The backend accepts any version in [MinSupportedVersion, Version] so an older
// agent binary keeps working after a backend deploy. This matters because agents
// are installed on user machines and cannot be upgraded in lockstep.
const (
	Version             = 1
	MinSupportedVersion = 1
)

// Timing constants. Both peers derive their timers from these so the agent and
// the gateway can never disagree about what counts as a dead connection.
const (
	// HeartbeatInterval is how often a peer sends PING. Mandated by PROJECT.md.
	HeartbeatInterval = 30 * time.Second

	// HeartbeatTimeout is how long a peer waits for PONG before treating the
	// connection as dead. Set to 2.5x the interval so a single dropped
	// heartbeat does not tear down an otherwise healthy connection.
	HeartbeatTimeout = 75 * time.Second

	// MaxMessageBytes caps a single inbound frame. Log lines are the largest
	// legitimate message; anything beyond this is treated as abuse and the
	// connection is closed.
	MaxMessageBytes = 1 << 20 // 1 MiB

	// MaxClockSkew bounds how far an envelope timestamp may sit from the
	// receiver's clock. Combined with the AUTH nonce this provides replay
	// protection: a captured frame is only replayable inside this window, and
	// within the window the nonce cache rejects it.
	MaxClockSkew = 2 * time.Minute
)

// MessageType enumerates every message the protocol carries.
//
// This set is closed and matches PROJECT.md exactly. Note there is deliberately
// no dedicated AUTH_OK: a successful AUTH is answered with Ack (carrying the
// AUTH envelope's ID), and a failed one with Error. Reusing Ack keeps the type
// set minimal and gives every request-shaped message one uniform reply shape.
type MessageType string

const (
	// Agent -> backend.
	TypeAuth         MessageType = "AUTH"
	TypeStatus       MessageType = "STATUS"
	TypeLog          MessageType = "LOG"
	TypeTaskComplete MessageType = "TASK_COMPLETE"
	TypeTaskWaiting  MessageType = "TASK_WAITING"

	// Backend -> agent.
	TypePrompt MessageType = "PROMPT"

	// Backend -> dashboard.
	TypeDeviceOnline  MessageType = "DEVICE_ONLINE"
	TypeDeviceOffline MessageType = "DEVICE_OFFLINE"
	TypeNotification  MessageType = "NOTIFICATION"

	// Bidirectional.
	TypePing  MessageType = "PING"
	TypePong  MessageType = "PONG"
	TypeError MessageType = "ERROR"
	TypeAck   MessageType = "ACK"
)

// validTypes is the authoritative membership set for MessageType.
var validTypes = map[MessageType]struct{}{
	TypeAuth: {}, TypeStatus: {}, TypeLog: {}, TypeTaskComplete: {},
	TypeTaskWaiting: {}, TypePrompt: {}, TypeDeviceOnline: {},
	TypeDeviceOffline: {}, TypeNotification: {}, TypePing: {}, TypePong: {},
	TypeError: {}, TypeAck: {},
}

// Valid reports whether t is a member of the closed MessageType set.
func (t MessageType) Valid() bool {
	_, ok := validTypes[t]
	return ok
}

func (t MessageType) String() string { return string(t) }

// Envelope is the outer frame of every message on the wire.
//
// Payload is held as json.RawMessage so the transport layer can route and
// validate a frame without knowing the concrete payload type, and so an unknown
// message type can be logged and skipped rather than failing the whole
// connection. Decode extracts the typed payload at the point of use.
type Envelope struct {
	// Version is the protocol version of this frame.
	Version int `json:"v"`

	// ID uniquely identifies this message. Used to correlate Ack replies and to
	// deduplicate messages redelivered after a reconnect.
	ID string `json:"id"`

	Type MessageType `json:"type"`

	// Timestamp is when the sender produced the frame (UTC).
	Timestamp time.Time `json:"ts"`

	// Sequence is a per-connection monotonic counter. It lets the receiver
	// detect gaps and lets the dashboard order log lines that arrive out of
	// order. Zero means "unsequenced".
	Sequence uint64 `json:"seq,omitempty"`

	DeviceID  string `json:"device_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`

	// CorrelationID ties a message to the causal chain that produced it — for
	// example a PROMPT and every LOG line that results from it. Carried into
	// structured logs so one dashboard action is traceable end to end.
	CorrelationID string `json:"correlation_id,omitempty"`

	Payload json.RawMessage `json:"payload,omitempty"`
}

// ErrUnsupportedVersion is returned when a peer speaks a protocol version this
// build cannot handle.
var ErrUnsupportedVersion = errors.New("protocol: unsupported version")

// Validate checks the envelope's structural invariants.
//
// It deliberately does NOT check the payload: payload validation belongs to the
// handler that knows the concrete type. Keeping the two separate means the
// transport can reject malformed frames cheaply, before any per-type work.
func (e *Envelope) Validate() error {
	if e.Version < MinSupportedVersion || e.Version > Version {
		return fmt.Errorf("%w: got %d, supported %d..%d",
			ErrUnsupportedVersion, e.Version, MinSupportedVersion, Version)
	}
	if e.ID == "" {
		return errors.New("protocol: envelope id is required")
	}
	if !e.Type.Valid() {
		return fmt.Errorf("protocol: unknown message type %q", e.Type)
	}
	if e.Timestamp.IsZero() {
		return errors.New("protocol: envelope timestamp is required")
	}
	return nil
}

// FreshWithin reports whether the envelope timestamp is within skew of now.
//
// Replay protection, part one: a frame captured off the wire is only accepted
// inside a narrow time window. Part two is the AUTH nonce cache in the backend,
// which rejects a replay that arrives inside that window.
func (e *Envelope) FreshWithin(now time.Time, skew time.Duration) bool {
	delta := now.Sub(e.Timestamp)
	if delta < 0 {
		delta = -delta
	}
	return delta <= skew
}

// Decode unmarshals the envelope payload into T.
//
// Generic so callers get a typed payload with no per-type boilerplate and no
// interface{} assertions at call sites:
//
//	p, err := protocol.Decode[protocol.PromptPayload](env)
func Decode[T any](e Envelope) (T, error) {
	var out T
	if len(e.Payload) == 0 {
		return out, fmt.Errorf("protocol: %s envelope has empty payload", e.Type)
	}
	if err := json.Unmarshal(e.Payload, &out); err != nil {
		return out, fmt.Errorf("protocol: decode %s payload: %w", e.Type, err)
	}
	return out, nil
}

// NewEnvelope builds a validated envelope carrying payload.
//
// id is supplied by the caller rather than generated here so this package stays
// free of an ID-generation dependency and so tests can produce deterministic
// frames. Use the id package to mint one.
func NewEnvelope(id string, t MessageType, now time.Time, payload any) (Envelope, error) {
	e := Envelope{
		Version:   Version,
		ID:        id,
		Type:      t,
		Timestamp: now.UTC(),
	}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return Envelope{}, fmt.Errorf("protocol: encode %s payload: %w", t, err)
		}
		e.Payload = raw
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}
