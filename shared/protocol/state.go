package protocol

// AgentState is the lifecycle state of a supervised coding agent.
//
// Modelled as an explicit state machine rather than a set of booleans
// (isRunning, isWaiting, hasCrashed) because booleans permit contradictory
// combinations that then have to be defended against everywhere. One enum plus a
// declared transition table makes an invalid state unrepresentable, and lets the
// dashboard render from a single field.
type AgentState string

const (
	// StateIdle means no coding agent process is running.
	StateIdle AgentState = "idle"
	// StateStarting means the process is launching but has not yet produced
	// output confirming readiness.
	StateStarting AgentState = "starting"
	// StateRunning means the coding agent is actively working on a task.
	StateRunning AgentState = "running"
	// StateWaitingInput means it is blocked on human input. This is the state
	// that triggers a user notification.
	StateWaitingInput AgentState = "waiting_input"
	// StateStopping means a graceful shutdown is in progress.
	StateStopping AgentState = "stopping"
	// StateStopped means the process exited as instructed.
	StateStopped AgentState = "stopped"
	// StateCrashed means the process exited unexpectedly. Distinguished from
	// StateStopped so the agent knows whether to attempt recovery.
	StateCrashed AgentState = "crashed"
)

// allowedTransitions declares the legal state machine.
//
// Keeping this as data rather than a switch statement means the session manager
// can validate a transition, and a test can assert the machine has no
// unreachable states, without duplicating the rules.
var allowedTransitions = map[AgentState][]AgentState{
	StateIdle:         {StateStarting},
	StateStarting:     {StateRunning, StateCrashed, StateStopped},
	StateRunning:      {StateWaitingInput, StateStopping, StateStopped, StateCrashed},
	StateWaitingInput: {StateRunning, StateStopping, StateStopped, StateCrashed},
	StateStopping:     {StateStopped, StateCrashed},
	// Terminal states re-enter the machine only by starting a new session.
	StateStopped: {StateStarting},
	StateCrashed: {StateStarting},
}

// CanTransitionTo reports whether moving from s to next is legal.
func (s AgentState) CanTransitionTo(next AgentState) bool {
	for _, allowed := range allowedTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Active reports whether a coding session is live.
//
// This is the predicate that drives power management: the system is kept awake
// exactly while this is true, so a finished session cannot leave the user's
// machine pinned awake indefinitely.
func (s AgentState) Active() bool {
	return s == StateStarting || s == StateRunning || s == StateWaitingInput
}

// Terminal reports whether s is an end state for the current session.
func (s AgentState) Terminal() bool {
	return s == StateStopped || s == StateCrashed
}

func (s AgentState) String() string { return string(s) }

// ErrorCode is a stable, machine-readable failure identifier.
//
// Clients branch on these codes, so they are part of the protocol's compatibility
// surface: a code may be added, but never renamed or repurposed.
type ErrorCode string

const (
	// ErrCodeUnauthorized means the device token is missing, malformed, or
	// expired. Not retryable without new credentials.
	ErrCodeUnauthorized ErrorCode = "unauthorized"
	// ErrCodeForbidden means the token is valid but not for this resource.
	ErrCodeForbidden ErrorCode = "forbidden"
	// ErrCodeVersionUnsupported means the peer's protocol version is outside the
	// supported range. Not retryable; the agent must be upgraded.
	ErrCodeVersionUnsupported ErrorCode = "version_unsupported"
	// ErrCodeReplayDetected means the nonce was already used or the timestamp
	// fell outside the freshness window.
	ErrCodeReplayDetected ErrorCode = "replay_detected"
	// ErrCodeRateLimited means the peer exceeded its quota. Retryable after a
	// backoff.
	ErrCodeRateLimited ErrorCode = "rate_limited"
	// ErrCodeMalformed means the frame failed structural or payload validation.
	ErrCodeMalformed ErrorCode = "malformed"
	// ErrCodeDeviceNotFound means the device ID is unknown or was revoked.
	ErrCodeDeviceNotFound ErrorCode = "device_not_found"
	// ErrCodeSessionNotFound means the referenced session does not exist.
	ErrCodeSessionNotFound ErrorCode = "session_not_found"
	// ErrCodeAdapterUnavailable means the requested coding agent is not
	// installed or not supported by this agent build.
	ErrCodeAdapterUnavailable ErrorCode = "adapter_unavailable"
	// ErrCodeInternal is an unexpected server-side failure. Retryable.
	ErrCodeInternal ErrorCode = "internal"
)

// Retryable reports whether backing off and retrying could plausibly succeed.
//
// The agent's reconnect loop consults this to decide between retrying and
// stopping. Getting it wrong in the permissive direction means an agent with a
// revoked token retries forever and becomes a denial-of-service against our own
// gateway, so the default is deliberately "not retryable".
func (c ErrorCode) Retryable() bool {
	switch c {
	case ErrCodeRateLimited, ErrCodeInternal:
		return true
	default:
		return false
	}
}

func (c ErrorCode) String() string { return string(c) }
