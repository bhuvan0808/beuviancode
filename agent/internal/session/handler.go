package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/bhuvan0808/beuviancode/agent/internal/coding"
	"github.com/bhuvan0808/beuviancode/agent/internal/store"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// HandleFrame processes an inbound frame from the backend.
//
// Implements transport.Handler. Errors are returned for logging but never tear
// down the connection: one malformed frame must not cost the user their session.
func (m *Manager) HandleFrame(ctx context.Context, env protocol.Envelope) error {
	if env.CorrelationID != "" {
		// Carry the correlation ID so local logs join the chain that started with
		// a dashboard click.
		ctx = blog.WithCorrelationID(ctx, env.CorrelationID)
	}

	switch env.Type {
	case protocol.TypePrompt:
		return m.handlePrompt(ctx, env)

	case protocol.TypeNotification:
		return m.handleNotification(ctx, env)

	case protocol.TypeAck:
		// The backend acknowledged something we sent. Nothing to do: our outbound
		// frames are fire-and-forget, and delivery is tracked server-side.
		return nil

	case protocol.TypeError:
		payload, err := protocol.Decode[protocol.ErrorPayload](env)
		if err != nil {
			return err
		}
		m.log.Warn("backend reported an error",
			slog.String("code", payload.Code.String()),
			slog.String("message", payload.Message),
			slog.Bool("retryable", payload.Retryable))
		return nil

	default:
		// A frame type we do not handle, or one from a newer protocol version.
		// Ignored rather than fatal, so a forward-compatible backend does not
		// break older agents.
		m.log.Debug("ignoring unexpected frame", slog.String("type", env.Type.String()))
		return nil
	}
}

// handlePrompt injects a prompt into the coding agent.
//
// The delivery contract: acknowledge only what was actually injected. A prompt the
// coding agent could not accept is persisted locally and retried, because the
// alternative — acknowledging it and dropping it — loses the user's instruction
// silently, which is the one failure this product cannot have.
func (m *Manager) handlePrompt(ctx context.Context, env protocol.Envelope) error {
	payload, err := protocol.Decode[protocol.PromptPayload](env)
	if err != nil {
		return err
	}

	m.mu.RLock()
	adapter := m.adapter
	m.mu.RUnlock()

	// No session running. Persist it so it survives an agent restart and is
	// injected when a session starts.
	if adapter == nil {
		if err := m.queuePrompt(payload); err != nil {
			m.log.Error("failed to persist a prompt for a device with no session", blog.Err(err))
			m.ackPrompt(env.ID, payload.PromptID, false, "no session is running and the prompt could not be stored")
			return err
		}
		m.log.Info("prompt stored; no session is running",
			slog.String("prompt_id", payload.PromptID))
		// Acknowledged: it is durably ours now, so the backend should stop
		// redelivering it.
		m.ackPrompt(env.ID, payload.PromptID, true, "")
		m.sendStatus(ctx)
		return nil
	}

	if err := adapter.SendPrompt(ctx, payload.Text); err != nil {
		// ErrNotAcceptingInput means the coding agent is mid-task or still
		// starting. Queue and retry rather than discarding.
		if errors.Is(err, coding.ErrNotAcceptingInput) || errors.Is(err, coding.ErrNotRunning) {
			if qerr := m.queuePrompt(payload); qerr != nil {
				m.ackPrompt(env.ID, payload.PromptID, false, "agent busy and the prompt could not be stored")
				return qerr
			}
			m.log.Info("coding agent is not accepting input; prompt queued locally",
				slog.String("prompt_id", payload.PromptID))
			m.ackPrompt(env.ID, payload.PromptID, true, "")
			return nil
		}
		// A genuine failure. NOT acknowledged, so the backend redelivers.
		m.ackPrompt(env.ID, payload.PromptID, false, err.Error())
		return err
	}

	m.log.Info("prompt injected",
		slog.String("prompt_id", payload.PromptID),
		slog.Int("attempt", payload.Attempt),
		slog.String("correlation_id", env.CorrelationID))

	m.ackPrompt(env.ID, payload.PromptID, true, "")
	m.sendStatus(ctx)
	return nil
}

// handleNotification processes a control instruction from the backend.
//
// The protocol's message set is closed at 13 types, so control actions that do not
// warrant a new type arrive as notifications with a machine-readable `kind`. That
// is precisely why `kind` is a stable identifier rather than a display label.
func (m *Manager) handleNotification(ctx context.Context, env protocol.Envelope) error {
	payload, err := protocol.Decode[protocol.NotificationPayload](env)
	if err != nil {
		return err
	}

	switch payload.Kind {
	case "session_stop_requested":
		m.log.Info("stop requested by the backend", slog.String("session_id", env.SessionID))
		return m.StopSession(ctx)
	default:
		// An informational notification meant for the dashboard, echoed to us.
		m.log.Debug("notification received", slog.String("kind", payload.Kind))
		return nil
	}
}

// ackPrompt tells the backend whether a prompt was accepted.
//
// AckID carries the PROMPT ID rather than the envelope ID, because that is what
// the backend's queue is keyed on — it is what lets a prompt be marked delivered
// exactly once.
func (m *Manager) ackPrompt(_, promptID string, accepted bool, reason string) {
	env, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypeAck, time.Now().UTC(),
		protocol.AckPayload{AckID: promptID, Accepted: accepted, Reason: reason})
	if err != nil {
		return
	}
	if err := m.sender.Send(env); err != nil {
		m.log.Debug("failed to queue ACK", blog.Err(err))
	}
}

// queuePrompt persists a prompt for later injection.
func (m *Manager) queuePrompt(p protocol.PromptPayload) error {
	return m.store.Update(func(st *store.State) {
		// Deduplicate: a redelivered prompt must not be queued twice, or the
		// coding agent would receive the same instruction repeatedly.
		for _, existing := range st.PendingPrompts {
			if existing.PromptID == p.PromptID {
				return
			}
		}
		// Bounded, so a long outage cannot grow the state file without limit. The
		// OLDEST is dropped: a stale instruction is less valuable than a fresh one.
		if len(st.PendingPrompts) >= m.cfg.Queue.MaxOfflinePrompts {
			st.PendingPrompts = st.PendingPrompts[1:]
			m.log.Warn("offline prompt queue is full; dropped the oldest prompt")
		}
		st.PendingPrompts = append(st.PendingPrompts, store.PendingPrompt{
			PromptID:   p.PromptID,
			Text:       p.Text,
			ReceivedAt: time.Now().UTC(),
		})
	})
}

// drainPendingPrompts injects prompts queued while no session was running.
//
// Called when a session starts and after a reconnect. Stops at the first failure
// rather than continuing: if the coding agent stopped accepting input, the rest
// would fail too, and burning their attempt counters achieves nothing.
func (m *Manager) drainPendingPrompts(ctx context.Context) {
	m.mu.RLock()
	adapter := m.adapter
	m.mu.RUnlock()
	if adapter == nil {
		return
	}

	pending := m.store.Current().PendingPrompts
	if len(pending) == 0 {
		return
	}

	injected := 0
	for _, p := range pending {
		if err := adapter.SendPrompt(ctx, p.Text); err != nil {
			m.log.Debug("stopping prompt drain", blog.Err(err))
			break
		}
		injected++
		// Injecting several prompts back to back would interleave them in the
		// coding agent's input. A brief pause lets it consume each one.
		time.Sleep(500 * time.Millisecond)
	}

	if injected == 0 {
		return
	}

	if err := m.store.Update(func(st *store.State) {
		if injected >= len(st.PendingPrompts) {
			st.PendingPrompts = nil
			return
		}
		st.PendingPrompts = st.PendingPrompts[injected:]
	}); err != nil {
		m.log.Warn("failed to clear injected prompts from the queue", blog.Err(err))
	}

	m.log.Info("injected queued prompts", slog.Int("count", injected))
	m.sendStatus(ctx)
}

// OnConnected fires after a successful handshake.
//
// Resending status immediately matters: the dashboard may have been showing stale
// state for the whole outage, and waiting for the next timer tick would prolong it.
func (m *Manager) OnConnected(ctx context.Context) {
	m.log.Info("connected to the backend")
	m.sendStatus(ctx)
	m.drainPendingPrompts(ctx)
}

// OnDisconnected fires when the connection drops.
//
// Deliberately does nothing to the session. A dropped connection is not a reason
// to stop the user's coding agent — that is the entire point of the offline queue,
// and stopping work because monitoring went away would be the wrong trade.
func (m *Manager) OnDisconnected(_ context.Context, err error) {
	if err != nil {
		m.log.Warn("disconnected from the backend; the session continues", blog.Err(err))
		return
	}
	m.log.Info("disconnected from the backend; the session continues")
}
