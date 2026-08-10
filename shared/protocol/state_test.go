package protocol_test

import (
	"testing"

	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

func TestAgentStateMachine(t *testing.T) {
	tests := []struct {
		from, to protocol.AgentState
		allowed  bool
	}{
		{protocol.StateIdle, protocol.StateStarting, true},
		{protocol.StateStarting, protocol.StateRunning, true},
		{protocol.StateRunning, protocol.StateWaitingInput, true},
		{protocol.StateWaitingInput, protocol.StateRunning, true},
		{protocol.StateRunning, protocol.StateCrashed, true},
		{protocol.StateStopped, protocol.StateStarting, true},
		{protocol.StateCrashed, protocol.StateStarting, true},

		// Illegal jumps. Idle -> Running would skip process launch entirely and
		// would let the dashboard claim work is happening with no process.
		{protocol.StateIdle, protocol.StateRunning, false},
		{protocol.StateIdle, protocol.StateWaitingInput, false},
		{protocol.StateStopped, protocol.StateRunning, false},
		{protocol.StateRunning, protocol.StateStarting, false},
	}
	for _, tc := range tests {
		if got := tc.from.CanTransitionTo(tc.to); got != tc.allowed {
			t.Errorf("%s -> %s: got %v, want %v", tc.from, tc.to, got, tc.allowed)
		}
	}
}

func TestAgentStateActiveDrivesPowerManagement(t *testing.T) {
	// Active() is what holds the sleep lock. If a terminal state ever reported
	// active, a finished session would pin the user's machine awake forever.
	active := map[protocol.AgentState]bool{
		protocol.StateIdle:         false,
		protocol.StateStarting:     true,
		protocol.StateRunning:      true,
		protocol.StateWaitingInput: true,
		protocol.StateStopping:     false,
		protocol.StateStopped:      false,
		protocol.StateCrashed:      false,
	}
	for state, want := range active {
		if got := state.Active(); got != want {
			t.Errorf("%s.Active() = %v, want %v", state, got, want)
		}
	}
	for _, s := range []protocol.AgentState{protocol.StateStopped, protocol.StateCrashed} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
		if s.Active() {
			t.Errorf("%s is terminal and must not be active", s)
		}
	}
}

func TestErrorCodeRetryability(t *testing.T) {
	// Getting this wrong permissively turns our own agents into a DoS against
	// the gateway, so assert the closed set explicitly.
	retryable := map[protocol.ErrorCode]bool{
		protocol.ErrCodeRateLimited: true,
		protocol.ErrCodeInternal:    true,

		protocol.ErrCodeUnauthorized:       false,
		protocol.ErrCodeForbidden:          false,
		protocol.ErrCodeVersionUnsupported: false,
		protocol.ErrCodeReplayDetected:     false,
		protocol.ErrCodeMalformed:          false,
		protocol.ErrCodeDeviceNotFound:     false,
		protocol.ErrCodeSessionNotFound:    false,
		protocol.ErrCodeAdapterUnavailable: false,
	}
	for code, want := range retryable {
		if got := code.Retryable(); got != want {
			t.Errorf("%s.Retryable() = %v, want %v", code, got, want)
		}
	}
	// An unrecognised code must default to non-retryable.
	if protocol.ErrorCode("something_new").Retryable() {
		t.Error("unknown error codes must default to non-retryable")
	}
}
