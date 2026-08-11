package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// PromptService queues and forwards prompts to devices.
//
// This is the operation the whole product exists for: a user types an instruction
// on a phone and the coding agent on their laptop carries it out. The correctness
// property that matters most is that a submitted prompt is never silently lost.
type PromptService struct {
	prompts    port.PromptQueueStore
	devices    port.DeviceStore
	sessions   port.SessionStore
	messages   port.MessageStore
	dispatcher port.PromptDispatcher
	conns      port.ConnectionRegistry
	audit      port.AuditLogger
	ids        port.IDGenerator
	clock      port.Clock
	log        *slog.Logger
}

// PromptDeps groups PromptService's collaborators.
type PromptDeps struct {
	Prompts    port.PromptQueueStore
	Devices    port.DeviceStore
	Sessions   port.SessionStore
	Messages   port.MessageStore
	Dispatcher port.PromptDispatcher
	Conns      port.ConnectionRegistry
	Audit      port.AuditLogger
	IDs        port.IDGenerator
	Clock      port.Clock
	Log        *slog.Logger
}

// NewPromptService builds a PromptService.
func NewPromptService(d PromptDeps) *PromptService {
	return &PromptService{
		prompts: d.Prompts, devices: d.Devices, sessions: d.Sessions,
		messages: d.Messages, dispatcher: d.Dispatcher, conns: d.Conns,
		audit: d.Audit, ids: d.IDs, clock: d.Clock,
		log: d.Log.With(slog.String("service", "prompt")),
	}
}

// SendInput describes a prompt submission.
type SendInput struct {
	UserID    string
	DeviceID  string
	SessionID string
	Text      string
	IPAddress string
}

// Send queues a prompt and attempts immediate delivery.
//
// The ordering here is the single most important sequence in the backend:
//
//  1. Verify the device belongs to the caller.
//  2. COMMIT the prompt to PostgreSQL.
//  3. Only then attempt Redis dispatch and a direct socket write.
//
// Step 2 completing before the caller is acknowledged is the whole guarantee. Once
// the API returns 202, the instruction exists durably; if step 3 fails entirely,
// the prompt is still delivered from the database on the agent's next reconnect.
//
// Reversing 2 and 3 for lower latency would create a window where the user is told
// their prompt was accepted while the only copy lives in a cache. That window is
// small and rarely hit, which is exactly what makes it the kind of bug that
// survives to production.
//
// An offline device is deliberately NOT an error. Queueing for a sleeping laptop
// is the product working as intended, not a failure to report.
func (s *PromptService) Send(ctx context.Context, in SendInput) (domain.QueuedPrompt, error) {
	now := s.clock.Now()

	// Ownership check first: never queue work against a device the caller does not
	// own, and never disclose whether an unowned device exists.
	device, err := s.devices.ByIDForUser(ctx, in.DeviceID, in.UserID)
	if err != nil {
		return domain.QueuedPrompt{}, err
	}
	if device.Revoked() {
		return domain.QueuedPrompt{}, domain.ErrDeviceRevoked
	}

	correlationID := blog.CorrelationIDFrom(ctx)
	if correlationID == "" {
		correlationID = s.ids.NewID(id.PrefixCorrelation)
	}

	prompt := domain.QueuedPrompt{
		ID:            s.ids.NewID(id.PrefixPrompt),
		UserID:        in.UserID,
		DeviceID:      in.DeviceID,
		SessionID:     in.SessionID,
		Text:          in.Text,
		Status:        domain.PromptPending,
		EnqueuedAt:    now,
		CorrelationID: correlationID,
		CreatedAt:     now,
	}
	if err := prompt.Validate(); err != nil {
		return domain.QueuedPrompt{}, err
	}

	// THE durability point. Everything after this is best-effort.
	if err := s.prompts.Enqueue(ctx, prompt); err != nil {
		return domain.QueuedPrompt{}, err
	}

	// Record it in the conversation so the dashboard shows the instruction
	// immediately, before the agent has acted on it.
	if in.SessionID != "" {
		if err := s.messages.Create(ctx, domain.Message{
			ID:        s.ids.NewID(id.PrefixMessage),
			SessionID: in.SessionID,
			UserID:    in.UserID,
			Role:      domain.RoleUser,
			Content:   in.Text,
			PromptID:  prompt.ID,
			CreatedAt: now,
		}); err != nil {
			// Not fatal: the prompt is already durable, and a missing conversation
			// entry is cosmetic compared with losing the instruction.
			s.log.Warn("failed to record prompt message", blog.Err(err))
		}
	}

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: in.UserID, Action: domain.ActionPromptSent,
		TargetType: "prompt", TargetID: prompt.ID,
		Metadata:  map[string]any{"device_id": in.DeviceID, "length": len(in.Text)},
		IPAddress: in.IPAddress, CorrelationID: correlationID, CreatedAt: now,
	})

	// Best-effort immediate delivery, in preference order: this instance's own
	// socket first (no network hop), then Redis for another instance.
	s.tryDeliver(ctx, prompt)

	s.log.Info("prompt queued",
		slog.String("prompt_id", prompt.ID),
		slog.String("device_id", in.DeviceID),
		slog.String("correlation_id", correlationID))

	return prompt, nil
}

// tryDeliver attempts immediate delivery without affecting the caller's result.
func (s *PromptService) tryDeliver(ctx context.Context, p domain.QueuedPrompt) {
	// Locally connected? Write straight to the socket.
	if s.conns.DeviceConnected(p.DeviceID) {
		if err := s.Deliver(ctx, p); err == nil {
			return
		}
	}
	// Otherwise let whichever instance owns the connection pick it up.
	if err := s.dispatcher.Publish(ctx, p.DeviceID, p.ID); err != nil {
		// Logged, never returned. The prompt is durable and will be delivered on
		// reconnect; surfacing this to the user would be alarming and useless.
		s.log.Warn("dispatch publish failed; prompt will be delivered on reconnect",
			slog.String("prompt_id", p.ID), blog.Err(err))
	}
}

// Deliver writes a prompt to a locally-connected device and records the attempt.
func (s *PromptService) Deliver(ctx context.Context, p domain.QueuedPrompt) error {
	env, err := protocol.NewEnvelope(
		s.ids.NewID(id.PrefixMessage), protocol.TypePrompt, s.clock.Now(),
		protocol.PromptPayload{
			PromptID:   p.ID,
			Text:       p.Text,
			EnqueuedAt: p.EnqueuedAt,
			Attempt:    p.Attempts + 1,
		})
	if err != nil {
		return err
	}
	env.DeviceID = p.DeviceID
	env.SessionID = p.SessionID
	env.CorrelationID = p.CorrelationID

	if !s.conns.SendToDevice(p.DeviceID, env) {
		return domain.ErrDeviceOffline
	}

	p.MarkDispatched(s.clock.Now())
	if err := s.prompts.Update(ctx, p); err != nil {
		// The frame is already on the wire. Failing to record the attempt means it
		// may be redelivered later, which the agent deduplicates by prompt ID.
		s.log.Warn("failed to record dispatch", slog.String("prompt_id", p.ID), blog.Err(err))
	}
	return nil
}

// DeliverPending flushes a device's queued prompts.
//
// Called when an agent connects or reconnects. This is the path that makes Redis
// disposable: anything the cache lost is found here, in order, from the
// authoritative store.
func (s *PromptService) DeliverPending(ctx context.Context, deviceID string) (int, error) {
	pending, err := s.prompts.PendingForDevice(ctx, deviceID, 50)
	if err != nil {
		return 0, err
	}

	delivered := 0
	for _, p := range pending {
		if p.Exhausted() {
			p.MarkFailed("exceeded maximum delivery attempts", s.clock.Now())
			if err := s.prompts.Update(ctx, p); err != nil {
				s.log.Warn("failed to mark prompt failed", blog.Err(err))
			}
			continue
		}
		if err := s.Deliver(ctx, p); err != nil {
			// The device went away mid-flush. Stop rather than continuing: the
			// rest are still queued and the next reconnect will retry them.
			break
		}
		delivered++
	}

	if delivered > 0 {
		s.log.Info("flushed queued prompts",
			slog.String("device_id", deviceID), slog.Int("count", delivered))
	}
	return delivered, nil
}

// DeliverByID delivers a specific prompt, used by the cross-instance dispatch path.
func (s *PromptService) DeliverByID(ctx context.Context, deviceID, promptID string) error {
	pending, err := s.prompts.PendingForDevice(ctx, deviceID, 50)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if p.ID == promptID {
			return s.Deliver(ctx, p)
		}
	}
	// Already delivered or cancelled by the time we got here. Not an error: the
	// dispatch signal and the database can legitimately disagree briefly.
	return nil
}

// Acknowledge records that an agent accepted a prompt.
//
// Idempotent, because a redelivered ACK after a reconnect must not corrupt the
// record or double-count anything.
func (s *PromptService) Acknowledge(ctx context.Context, deviceID, promptID string, accepted bool, reason string) error {
	pending, err := s.prompts.PendingForDevice(ctx, deviceID, 100)
	if err != nil {
		return err
	}

	for _, p := range pending {
		if p.ID != promptID {
			continue
		}
		now := s.clock.Now()
		if accepted {
			p.MarkDelivered(now)
			s.log.Info("prompt delivered",
				slog.String("prompt_id", p.ID),
				slog.String("correlation_id", p.CorrelationID))
		} else {
			// Rejected by the agent — usually because the coding agent is not
			// accepting input. Left pending so it is retried rather than dropped,
			// unless attempts are exhausted.
			if p.Exhausted() {
				p.MarkFailed(reason, now)
			} else {
				p.Status = domain.PromptPending
			}
		}
		return s.prompts.Update(ctx, p)
	}
	return nil // unknown or already-terminal prompt
}

// List returns a user's prompts.
func (s *PromptService) List(ctx context.Context, userID, deviceID string, p port.Page) ([]domain.QueuedPrompt, string, error) {
	return s.prompts.ListForUser(ctx, userID, deviceID, p)
}

// Cancel withdraws an undelivered prompt.
func (s *PromptService) Cancel(ctx context.Context, promptID, userID, ipAddress string) error {
	prompt, err := s.prompts.ByIDForUser(ctx, promptID, userID)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	if err := prompt.Cancel(now); err != nil {
		// Already delivered. An injected prompt cannot be un-injected — the coding
		// agent has acted on it.
		return err
	}
	if err := s.prompts.Update(ctx, prompt); err != nil {
		return err
	}
	s.audit.Record(ctx, domain.AuditEntry{
		UserID: userID, Action: domain.ActionPromptCancelled,
		TargetType: "prompt", TargetID: promptID,
		IPAddress: ipAddress, CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: now,
	})
	return nil
}

// Get returns one prompt.
func (s *PromptService) Get(ctx context.Context, promptID, userID string) (domain.QueuedPrompt, error) {
	return s.prompts.ByIDForUser(ctx, promptID, userID)
}

// ReconcilePending redelivers prompts whose dispatch signal was lost.
//
// The safety net for ADR-0006's "Redis is disposable" claim. Without a periodic
// sweep, a prompt enqueued while Redis was briefly down would sit pending until
// the agent happened to reconnect — which for an always-on machine could be days.
//
// Guarded by a distributed lock at the call site so only one instance sweeps.
func (s *PromptService) ReconcilePending(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := s.clock.Now().Add(-olderThan)
	redelivered := 0

	// Only devices with a live connection on THIS instance are worth sweeping:
	// anything else has no socket to write to.
	for _, deviceID := range s.connectedDevices() {
		pending, err := s.prompts.PendingForDevice(ctx, deviceID, 20)
		if err != nil {
			continue
		}
		for _, p := range pending {
			// Skip anything recent — it is probably mid-flight.
			if p.EnqueuedAt.After(cutoff) {
				continue
			}
			if p.Exhausted() {
				p.MarkFailed("exceeded maximum delivery attempts", s.clock.Now())
				_ = s.prompts.Update(ctx, p)
				continue
			}
			if err := s.Deliver(ctx, p); err == nil {
				redelivered++
			}
		}
	}
	if redelivered > 0 {
		s.log.Info("reconciled stalled prompts", slog.Int("count", redelivered))
	}
	return redelivered, nil
}

// connectedDevices is a seam for the reconciliation sweep.
//
// The ConnectionRegistry port deliberately does not expose an enumeration method,
// because nothing on the hot path needs one and adding it would invite callers to
// iterate connections instead of addressing them directly.
func (s *PromptService) connectedDevices() []string {
	type enumerator interface{ ConnectedDevices() []string }
	if e, ok := s.conns.(enumerator); ok {
		return e.ConnectedDevices()
	}
	return nil
}
