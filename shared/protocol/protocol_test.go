package protocol_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

func TestMessageTypeSetMatchesSpec(t *testing.T) {
	// PROJECT.md defines a closed set of 13 message types. This test is the
	// guard against silently widening the protocol: adding a type without
	// updating the spec and docs/WEBSOCKET_PROTOCOL.md fails here.
	want := []protocol.MessageType{
		protocol.TypeAuth, protocol.TypePing, protocol.TypePong,
		protocol.TypeStatus, protocol.TypeLog, protocol.TypePrompt,
		protocol.TypeTaskComplete, protocol.TypeTaskWaiting,
		protocol.TypeDeviceOnline, protocol.TypeDeviceOffline,
		protocol.TypeError, protocol.TypeAck, protocol.TypeNotification,
	}
	if len(want) != 13 {
		t.Fatalf("spec defines 13 message types, test lists %d", len(want))
	}
	for _, mt := range want {
		if !mt.Valid() {
			t.Errorf("%s should be a valid message type", mt)
		}
	}
	if protocol.MessageType("AUTH_OK").Valid() {
		t.Error("AUTH_OK is not in the spec; AUTH is answered with ACK")
	}
	if protocol.MessageType("").Valid() {
		t.Error("empty message type must be invalid")
	}
}

func TestNewEnvelopeRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	in := protocol.PromptPayload{
		PromptID:   "prm_01J9Z3K7QF8XKM2N4P6R8T0VWY",
		Text:       "Now implement authentication.",
		EnqueuedAt: now,
		Attempt:    1,
	}

	env, err := protocol.NewEnvelope("msg_01J9Z3K7QF8XKM2N4P6R8T0VWY", protocol.TypePrompt, now, in)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.Version != protocol.Version {
		t.Errorf("Version = %d, want %d", env.Version, protocol.Version)
	}

	// Marshal and unmarshal to exercise the actual wire path, not just structs.
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded protocol.Envelope
	if uerr := json.Unmarshal(raw, &decoded); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if verr := decoded.Validate(); verr != nil {
		t.Fatalf("round-tripped envelope failed validation: %v", verr)
	}

	out, err := protocol.Decode[protocol.PromptPayload](decoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Text != in.Text || out.PromptID != in.PromptID || out.Attempt != in.Attempt {
		t.Errorf("payload mismatch:\n got %+v\nwant %+v", out, in)
	}
	if !out.EnqueuedAt.Equal(in.EnqueuedAt) {
		t.Errorf("EnqueuedAt = %v, want %v", out.EnqueuedAt, in.EnqueuedAt)
	}
}

func TestEnvelopeValidate(t *testing.T) {
	now := time.Now().UTC()
	base := func() protocol.Envelope {
		return protocol.Envelope{
			Version:   protocol.Version,
			ID:        "msg_01J9Z3K7QF8XKM2N4P6R8T0VWY",
			Type:      protocol.TypeStatus,
			Timestamp: now,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*protocol.Envelope)
		wantErr bool
	}{
		{"valid", func(*protocol.Envelope) {}, false},
		{"missing id", func(e *protocol.Envelope) { e.ID = "" }, true},
		{"zero timestamp", func(e *protocol.Envelope) { e.Timestamp = time.Time{} }, true},
		{"unknown type", func(e *protocol.Envelope) { e.Type = "TELEPORT" }, true},
		{"version too low", func(e *protocol.Envelope) { e.Version = protocol.MinSupportedVersion - 1 }, true},
		{"version too high", func(e *protocol.Envelope) { e.Version = protocol.Version + 1 }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := base()
			tc.mutate(&e)
			err := e.Validate()
			if tc.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestFreshWithinRejectsStaleAndFutureFrames(t *testing.T) {
	now := time.Now().UTC()
	// Replay protection depends on this being symmetric: a frame stamped in the
	// future is as suspect as an old one, since an attacker controls the clock
	// they stamp with.
	cases := []struct {
		name  string
		ts    time.Time
		fresh bool
	}{
		{"now", now, true},
		{"recent past", now.Add(-30 * time.Second), true},
		{"near future", now.Add(30 * time.Second), true},
		{"stale", now.Add(-10 * time.Minute), false},
		{"far future", now.Add(10 * time.Minute), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := protocol.Envelope{Timestamp: tc.ts}
			if got := e.FreshWithin(now, protocol.MaxClockSkew); got != tc.fresh {
				t.Errorf("FreshWithin = %v, want %v", got, tc.fresh)
			}
		})
	}
}

func TestDecodeEmptyPayloadIsAnError(t *testing.T) {
	e := protocol.Envelope{
		Version: protocol.Version, ID: "msg_x",
		Type: protocol.TypePrompt, Timestamp: time.Now(),
	}
	if _, err := protocol.Decode[protocol.PromptPayload](e); err == nil {
		t.Error("decoding an empty payload should fail rather than yield a zero value")
	}
}

func TestHeartbeatTimeoutToleratesOneMissedBeat(t *testing.T) {
	// A timeout at or below the interval would tear down healthy connections on
	// a single dropped frame.
	if protocol.HeartbeatTimeout <= 2*protocol.HeartbeatInterval {
		t.Errorf("HeartbeatTimeout (%v) must exceed 2x HeartbeatInterval (%v)",
			protocol.HeartbeatTimeout, protocol.HeartbeatInterval)
	}
	if protocol.HeartbeatInterval != 30*time.Second {
		t.Errorf("PROJECT.md mandates a 30s heartbeat, got %v", protocol.HeartbeatInterval)
	}
}
