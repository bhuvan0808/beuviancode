package domain

import (
	"strings"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// Session is one supervised coding-agent run.
type Session struct {
	ID           string
	UserID       string
	DeviceID     string
	RepositoryID string

	Adapter string
	State   protocol.AgentState

	CurrentTask      string
	WorkingDirectory string

	PID int

	// ExitCode is nil while the process has not exited. A sentinel int cannot
	// express that: -1 is a real exit code on some platforms, and 0 would read as
	// "exited successfully".
	ExitCode *int

	StartedAt time.Time
	EndedAt   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate checks session invariants.
func (s *Session) Validate() error {
	if s.UserID == "" {
		return Invalid("user_id", "must be set")
	}
	if s.DeviceID == "" {
		return Invalid("device_id", "must be set")
	}
	if strings.TrimSpace(s.Adapter) == "" {
		return Invalid("adapter", "must not be empty")
	}
	if strings.TrimSpace(s.WorkingDirectory) == "" {
		// No default is possible: the coding agent writes files, so guessing
		// destroys real work.
		return Invalid("working_directory", "must not be empty")
	}
	return nil
}

// Active reports whether the session is still running.
//
// Derived from EndedAt rather than from State so a session whose final STATUS
// frame was lost is still correctly treated as active until explicitly ended.
func (s *Session) Active() bool { return s.EndedAt == nil }

// Elapsed returns how long the session ran, or has been running.
func (s *Session) Elapsed(now time.Time) time.Duration {
	if s.EndedAt != nil {
		return s.EndedAt.Sub(s.StartedAt)
	}
	return now.Sub(s.StartedAt)
}

// ApplyState transitions the session, rejecting illegal moves.
//
// Enforced here rather than trusting the agent's reported state: a buggy or
// malicious agent must not be able to drive the session into a state the machine
// forbids, because the dashboard renders directly from this field.
func (s *Session) ApplyState(next protocol.AgentState, now time.Time) error {
	if s.State == next {
		return nil // idempotent: a repeated STATUS frame is not an error
	}
	if !s.State.CanTransitionTo(next) {
		return Invalid("state", "illegal transition from "+s.State.String()+" to "+next.String())
	}
	s.State = next
	s.UpdatedAt = now

	// A terminal state closes the session. Doing this here rather than at call
	// sites means no path can leave a crashed session looking active forever.
	if next.Terminal() && s.EndedAt == nil {
		ended := now
		s.EndedAt = &ended
	}
	return nil
}

// End closes the session with an exit code.
func (s *Session) End(exitCode int, now time.Time) {
	if s.EndedAt == nil {
		ended := now
		s.EndedAt = &ended
	}
	s.ExitCode = &exitCode
	s.UpdatedAt = now
}

// SessionLog is a batch of output lines from a session.
//
// Uses a database sequence rather than a ULID: this is by far the
// highest-volume table, log rows are never referenced by external clients, and 8
// bytes versus 26 is material across millions of rows.
type SessionLog struct {
	ID        int64
	SessionID string

	// Seq is a per-session counter. Combined with a unique constraint it makes
	// ingestion idempotent, so a batch redelivered after a reconnect conflicts
	// instead of duplicating the transcript.
	Seq int64

	Stream  protocol.LogStream
	Content string

	// Truncated marks that lines were dropped upstream. Surfacing it is a
	// correctness requirement: a silently truncated transcript reads as complete.
	Truncated bool

	At        time.Time
	CreatedAt time.Time
}

// MessageRole identifies who produced a message.
type MessageRole string

const (
	RoleUser   MessageRole = "user"
	RoleAgent  MessageRole = "agent"
	RoleSystem MessageRole = "system"
)

// Message is an entry in the human-readable conversation for a session.
//
// Distinct from SessionLog: logs are raw process output, messages are the
// exchange the dashboard renders as a conversation.
type Message struct {
	ID        string
	SessionID string
	UserID    string // empty for agent-originated messages
	Role      MessageRole
	Content   string
	PromptID  string
	CreatedAt time.Time
}
