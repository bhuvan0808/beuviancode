package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gofiber/contrib/websocket"

	"github.com/bhuvan0808/beuviancode/backend/internal/app"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// dispatch routes an inbound frame to its handler.
//
// Every branch is restricted to device connections except PING. A dashboard client
// has the REST API for everything else, so accepting STATUS or LOG from a browser
// would let any authenticated user forge another device's telemetry.
func (g *Gateway) dispatch(ctx context.Context, conn *Conn, env protocol.Envelope, log *slog.Logger) error {
	// Carry the frame's correlation ID so downstream logs and audit entries join
	// up with the dashboard action that caused them.
	if env.CorrelationID != "" {
		ctx = blog.WithCorrelationID(ctx, env.CorrelationID)
	}

	switch env.Type {
	case protocol.TypePing:
		return g.handlePing(ctx, conn, env)

	case protocol.TypePong:
		// The peer answered our probe. Nothing to do: the read deadline was already
		// extended when the frame arrived.
		return nil

	case protocol.TypeStatus:
		return g.deviceOnly(conn, func() error { return g.handleStatus(ctx, conn, env, log) })

	case protocol.TypeLog:
		return g.deviceOnly(conn, func() error { return g.handleLog(ctx, conn, env) })

	case protocol.TypeTaskWaiting:
		return g.deviceOnly(conn, func() error { return g.handleTaskWaiting(ctx, conn, env, log) })

	case protocol.TypeTaskComplete:
		return g.deviceOnly(conn, func() error { return g.handleTaskComplete(ctx, conn, env, log) })

	case protocol.TypeAck:
		return g.deviceOnly(conn, func() error { return g.handleAck(ctx, conn, env) })

	case protocol.TypeAuth:
		// A second AUTH on an authenticated connection. Rejected rather than
		// re-authenticating, which would let a connection change identity mid-life.
		return fmt.Errorf("%w: already authenticated", domain.ErrConflict)

	case protocol.TypeError:
		// Agents report their own failures. Logged for diagnosis; no action.
		payload, err := protocol.Decode[protocol.ErrorPayload](env)
		if err == nil {
			log.Warn("agent reported an error",
				slog.String("code", payload.Code.String()),
				slog.String("message", payload.Message))
		}
		return nil

	default:
		// Server-to-client types arriving inbound, or a type from a newer protocol
		// version. Ignored rather than fatal, so a forward-compatible agent that
		// sends something new does not lose its connection.
		log.Debug("ignoring unexpected inbound frame", slog.String("type", env.Type.String()))
		return nil
	}
}

// deviceOnly rejects a frame that only an agent may send.
func (g *Gateway) deviceOnly(conn *Conn, fn func() error) error {
	if !conn.IsDevice() {
		return fmt.Errorf("%w: this frame may only be sent by a device", domain.ErrForbidden)
	}
	return fn()
}

// handlePing answers a heartbeat and refreshes presence.
//
// The PONG echoes the PING's nonce unchanged so the peer can match the reply to its
// own probe and measure round-trip latency rather than guessing.
func (g *Gateway) handlePing(ctx context.Context, conn *Conn, env protocol.Envelope) error {
	payload, err := protocol.Decode[protocol.HeartbeatPayload](env)
	if err != nil {
		// Tolerate a payload-less ping rather than dropping the connection: a
		// heartbeat is not worth failing a healthy session over.
		payload = protocol.HeartbeatPayload{}
	}

	if conn.IsDevice() {
		// The heartbeat is what keeps presence alive. Its TTL is 2.5x the interval,
		// so a single missed beat does not make a healthy device look offline.
		if err := g.devices.Touch(ctx, domain.Device{ID: conn.DeviceID, UserID: conn.UserID}); err != nil {
			g.log.Debug("failed to refresh presence", blog.Err(err))
		}
	}

	pong, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypePong, g.clock.Now(),
		protocol.HeartbeatPayload{Nonce: payload.Nonce, SentAt: g.clock.Now()})
	if err != nil {
		return err
	}
	return g.send(conn, pong)
}

// handleStatus records a device's reported state.
func (g *Gateway) handleStatus(ctx context.Context, conn *Conn, env protocol.Envelope, log *slog.Logger) error {
	payload, err := protocol.Decode[protocol.StatusPayload](env)
	if err != nil {
		return err
	}

	if err := g.devices.RecordStatus(ctx, conn.DeviceID, payload, env.SessionID, env.Timestamp); err != nil {
		return err
	}

	if env.SessionID != "" {
		// An out-of-order transition is logged and dropped inside ApplyStatus
		// rather than erroring: frames legitimately arrive out of order after a
		// reconnect, and tearing down the connection would be self-inflicted.
		if err := g.sessions.ApplyStatus(ctx, env.SessionID, payload.State, payload.CurrentTask); err != nil {
			log.Debug("could not apply session state", blog.Err(err))
		}
	}

	// Forward to the user's dashboards so the UI updates live.
	g.forwardToDashboards(ctx, conn.UserID, env)
	return nil
}

// handleLog stores a batch of output lines.
func (g *Gateway) handleLog(ctx context.Context, conn *Conn, env protocol.Envelope) error {
	if env.SessionID == "" {
		// Output with no session has nowhere to go. Not fatal: an agent may emit
		// startup chatter before a session exists.
		return nil
	}

	payload, err := protocol.Decode[protocol.LogPayload](env)
	if err != nil {
		return err
	}
	if err := g.sessions.AppendLogs(ctx, env.SessionID, payload); err != nil {
		return err
	}

	g.forwardToDashboards(ctx, conn.UserID, env)
	return nil
}

// handleTaskWaiting raises the notification the product exists to deliver.
//
// This is the moment that matters: the coding agent has stopped and needs a human.
// Everything else in the system is in service of getting this onto the user's phone
// promptly.
func (g *Gateway) handleTaskWaiting(ctx context.Context, conn *Conn, env protocol.Envelope, log *slog.Logger) error {
	payload, err := protocol.Decode[protocol.TaskWaitingPayload](env)
	if err != nil {
		return err
	}

	title := "Your coding agent is waiting for you"
	body := payload.Question
	if body == "" {
		switch payload.Reason {
		case protocol.WaitConfirmation:
			body = "It is waiting for you to confirm an action."
		case protocol.WaitError:
			body = "It stopped on an error and cannot continue on its own."
		default:
			body = "It finished and is ready for your next instruction."
		}
	}

	if _, err := g.notifs.Notify(ctx, app.NotifyInput{
		UserID:    conn.UserID,
		DeviceID:  conn.DeviceID,
		SessionID: env.SessionID,
		Kind:      domain.KindTaskWaiting,
		Title:     title,
		Body:      body,
		Severity:  protocol.SeverityInfo,
	}); err != nil {
		log.Warn("failed to raise waiting notification", blog.Err(err))
	}

	g.forwardToDashboards(ctx, conn.UserID, env)
	log.Info("coding agent is waiting for input",
		slog.String("reason", string(payload.Reason)),
		slog.String("session_id", env.SessionID))
	return nil
}

// handleTaskComplete records completion and notifies the user.
func (g *Gateway) handleTaskComplete(ctx context.Context, conn *Conn, env protocol.Envelope, log *slog.Logger) error {
	payload, err := protocol.Decode[protocol.TaskCompletePayload](env)
	if err != nil {
		return err
	}

	if env.SessionID != "" {
		if err := g.sessions.Complete(ctx, env.SessionID, payload.ExitCode, payload.Summary); err != nil {
			log.Warn("failed to record completion", blog.Err(err))
		}
	}

	severity := protocol.SeverityInfo
	title := "Your coding agent finished"
	// A non-zero exit code is a failure worth flagging differently. -1 means the
	// agent is merely idle between tasks, not that anything went wrong.
	if payload.ExitCode > 0 {
		severity = protocol.SeverityWarning
		title = "Your coding agent exited with an error"
	}

	if _, err := g.notifs.Notify(ctx, app.NotifyInput{
		UserID:    conn.UserID,
		DeviceID:  conn.DeviceID,
		SessionID: env.SessionID,
		Kind:      domain.KindTaskComplete,
		Title:     title,
		Body:      payload.Summary,
		Severity:  severity,
	}); err != nil {
		log.Warn("failed to raise completion notification", blog.Err(err))
	}

	g.forwardToDashboards(ctx, conn.UserID, env)
	log.Info("task complete",
		slog.Int("exit_code", payload.ExitCode),
		slog.Int64("duration_seconds", payload.DurationSeconds))
	return nil
}

// handleAck records that an agent accepted a prompt.
func (g *Gateway) handleAck(ctx context.Context, conn *Conn, env protocol.Envelope) error {
	payload, err := protocol.Decode[protocol.AckPayload](env)
	if err != nil {
		return err
	}
	// AckID carries the prompt ID the agent is acknowledging, which is what lets
	// the queue mark it delivered exactly once.
	return g.prompts.Acknowledge(ctx, conn.DeviceID, payload.AckID, payload.Accepted, payload.Reason)
}

// forwardToDashboards relays an agent frame to the user's dashboard clients.
//
// Goes through the event publisher rather than the local hub because the user's
// dashboard may be connected to a different backend instance. Failures are logged
// and swallowed: a dropped live update costs a stale dashboard until the next
// periodic STATUS frame, which is exactly why the agent re-sends status on a timer
// rather than only on transitions.
func (g *Gateway) forwardToDashboards(ctx context.Context, userID string, env protocol.Envelope) {
	if err := g.events.PublishToUser(ctx, userID, env); err != nil {
		g.log.Debug("failed to forward frame to dashboards", blog.Err(err))
	}
}

// send queues an envelope on a connection.
func (g *Gateway) send(conn *Conn, env protocol.Envelope) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("ws: encode %s: %w", env.Type, err)
	}
	if !conn.Send(payload) {
		conn.Close()
		return fmt.Errorf("ws: send buffer full")
	}
	return nil
}

// writeAck sends an ACK for a specific message.
//
// Written directly to the socket rather than queued, because it is used during the
// handshake before the writer goroutine exists.
func (g *Gateway) writeAck(ws *websocket.Conn, ackID string, accepted bool, reason string) {
	env, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypeAck, g.clock.Now(),
		protocol.AckPayload{AckID: ackID, Accepted: accepted, Reason: reason})
	if err != nil {
		return
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return
	}
	_ = ws.WriteMessage(websocket.TextMessage, payload)
}

// writeError sends an ERROR frame describing a failure.
//
// The retryable flag is what stops an agent with a revoked token retrying forever
// and becoming a denial-of-service against our own gateway, multiplied by every
// installed agent.
func (g *Gateway) writeError(ws *websocket.Conn, correlationID string, err error) {
	code := errorCode(err)
	env, buildErr := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypeError, g.clock.Now(),
		protocol.ErrorPayload{
			Code:      code,
			Message:   err.Error(),
			Retryable: code.Retryable(),
		})
	if buildErr != nil {
		return
	}
	env.CorrelationID = correlationID

	payload, mErr := json.Marshal(env)
	if mErr != nil {
		return
	}
	_ = ws.SetWriteDeadline(g.clock.Now().Add(5 * time.Second))
	_ = ws.WriteMessage(websocket.TextMessage, payload)
}

// errorCode maps a domain error to a protocol error code.
func errorCode(err error) protocol.ErrorCode {
	switch {
	case errors.Is(err, domain.ErrTokenExpired), errors.Is(err, domain.ErrUnauthorized):
		return protocol.ErrCodeUnauthorized
	case errors.Is(err, domain.ErrDeviceRevoked):
		return protocol.ErrCodeDeviceNotFound
	case errors.Is(err, domain.ErrForbidden):
		return protocol.ErrCodeForbidden
	case errors.Is(err, protocol.ErrUnsupportedVersion):
		return protocol.ErrCodeVersionUnsupported
	case errors.Is(err, domain.ErrValidation):
		return protocol.ErrCodeMalformed
	case errors.Is(err, domain.ErrNotFound):
		return protocol.ErrCodeSessionNotFound
	case errors.Is(err, domain.ErrRateLimited):
		return protocol.ErrCodeRateLimited
	case errors.Is(err, domain.ErrAdapterUnavailable):
		return protocol.ErrCodeAdapterUnavailable
	default:
		return protocol.ErrCodeInternal
	}
}
