package domain

import (
	"strings"
	"time"
)

// PromptStatus is the delivery lifecycle of a queued prompt.
type PromptStatus string

const (
	// PromptPending is durably stored and awaiting dispatch.
	PromptPending PromptStatus = "pending"
	// PromptDispatched has been sent to a device but not yet acknowledged.
	PromptDispatched PromptStatus = "dispatched"
	// PromptDelivered was acknowledged by the agent.
	PromptDelivered PromptStatus = "delivered"
	// PromptFailed exhausted its delivery attempts.
	PromptFailed PromptStatus = "failed"
	// PromptCancelled was withdrawn before delivery.
	PromptCancelled PromptStatus = "cancelled"
)

// Terminal reports whether the status is final.
func (s PromptStatus) Terminal() bool {
	return s == PromptDelivered || s == PromptFailed || s == PromptCancelled
}

// MaxPromptLength bounds a prompt.
//
// Generous enough for a detailed instruction, small enough that a single frame
// stays well under the protocol's 1 MiB limit even after JSON escaping.
const MaxPromptLength = 32 * 1024

// MaxDeliveryAttempts bounds redelivery before a prompt is marked failed.
//
// Bounded so a device that is permanently broken does not accumulate an infinite
// redelivery backlog. Five attempts spans a long enough window that an ordinary
// laptop reboot or network change is covered.
const MaxDeliveryAttempts = 5

// QueuedPrompt is a user instruction awaiting delivery to a device.
//
// PostgreSQL is the system of record for these; Redis only accelerates dispatch.
// The prompt is committed here before the API acknowledges the user, so a Redis
// failure costs latency and never the instruction itself. See ADR-0006.
type QueuedPrompt struct {
	ID        string
	UserID    string
	DeviceID  string
	SessionID string

	Text   string
	Status PromptStatus

	Attempts int

	EnqueuedAt   time.Time
	DispatchedAt *time.Time
	DeliveredAt  *time.Time

	Error string

	// CorrelationID ties this prompt to its whole causal chain across the
	// dashboard, backend, and agent, which is what makes "I sent a prompt and
	// nothing happened" answerable with one search.
	CorrelationID string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate checks prompt invariants.
func (p *QueuedPrompt) Validate() error {
	if p.UserID == "" {
		return Invalid("user_id", "must be set")
	}
	if p.DeviceID == "" {
		return Invalid("device_id", "must be set")
	}
	if strings.TrimSpace(p.Text) == "" {
		return Invalid("text", "must not be empty")
	}
	if len(p.Text) > MaxPromptLength {
		return Invalid("text", "exceeds the maximum length")
	}
	return nil
}

// MarkDispatched records a delivery attempt.
func (p *QueuedPrompt) MarkDispatched(now time.Time) {
	p.Status = PromptDispatched
	p.Attempts++
	p.DispatchedAt = &now
	p.UpdatedAt = now
}

// MarkDelivered records agent acknowledgement.
//
// Idempotent: a duplicate ACK after a reconnect must not corrupt the record or
// double-count anything.
func (p *QueuedPrompt) MarkDelivered(now time.Time) {
	if p.Status == PromptDelivered {
		return
	}
	p.Status = PromptDelivered
	p.DeliveredAt = &now
	p.UpdatedAt = now
}

// MarkFailed records a terminal delivery failure.
func (p *QueuedPrompt) MarkFailed(reason string, now time.Time) {
	p.Status = PromptFailed
	p.Error = reason
	p.UpdatedAt = now
}

// Cancel withdraws an undelivered prompt.
//
// Returns ErrConflict once delivered, because an injected prompt cannot be
// un-injected — the coding agent has already acted on it.
func (p *QueuedPrompt) Cancel(now time.Time) error {
	if p.Status == PromptDelivered {
		return ErrConflict
	}
	if p.Status.Terminal() {
		return ErrConflict
	}
	p.Status = PromptCancelled
	p.UpdatedAt = now
	return nil
}

// Exhausted reports whether delivery attempts are used up.
func (p *QueuedPrompt) Exhausted() bool { return p.Attempts >= MaxDeliveryAttempts }
