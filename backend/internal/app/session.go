package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// SessionService manages coding-session lifecycle, logs, and history.
type SessionService struct {
	sessions port.SessionStore
	logs     port.SessionLogStore
	messages port.MessageStore
	devices  port.DeviceStore
	repos    port.RepositoryStore
	conns    port.ConnectionRegistry
	events   port.EventPublisher
	audit    port.AuditLogger
	ids      port.IDGenerator
	clock    port.Clock
	log      *slog.Logger
}

// SessionDeps groups SessionService's collaborators.
type SessionDeps struct {
	Sessions port.SessionStore
	Logs     port.SessionLogStore
	Messages port.MessageStore
	Devices  port.DeviceStore
	Repos    port.RepositoryStore
	Conns    port.ConnectionRegistry
	Events   port.EventPublisher
	Audit    port.AuditLogger
	IDs      port.IDGenerator
	Clock    port.Clock
	Log      *slog.Logger
}

// NewSessionService builds a SessionService.
func NewSessionService(d SessionDeps) *SessionService {
	return &SessionService{
		sessions: d.Sessions, logs: d.Logs, messages: d.Messages, devices: d.Devices,
		repos: d.Repos, conns: d.Conns, events: d.Events, audit: d.Audit,
		ids: d.IDs, clock: d.Clock,
		log: d.Log.With(slog.String("service", "session")),
	}
}

// StartInput describes a session start request.
type StartInput struct {
	UserID           string
	DeviceID         string
	RepositoryID     string
	Adapter          string
	WorkingDirectory string
	InitialPrompt    string
	IPAddress        string
}

// Start begins a coding session on a device.
//
// Capability is checked here, before anything is persisted, so a user learns
// immediately that their device cannot run the requested coding agent. Discovering
// it later, after they have closed the laptop and walked away, is precisely the
// failure this product exists to prevent.
func (s *SessionService) Start(ctx context.Context, in StartInput) (domain.Session, error) {
	now := s.clock.Now()

	device, err := s.devices.ByIDForUser(ctx, in.DeviceID, in.UserID)
	if err != nil {
		return domain.Session{}, err
	}
	if device.Revoked() {
		return domain.Session{}, domain.ErrDeviceRevoked
	}

	// Capabilities may be empty on a device that registered but has not yet
	// completed a handshake. Only reject when we positively know the adapter is
	// missing; treating "unknown" as "unavailable" would block legitimate first
	// sessions.
	if len(device.Capabilities) > 0 && !device.Supports(in.Adapter) {
		return domain.Session{}, fmt.Errorf("%w: %s is not installed on %s",
			domain.ErrAdapterUnavailable, in.Adapter, device.Name)
	}

	// Reject a second concurrent session early with a clear message. The unique
	// partial index is the real guarantee, but it would surface as an opaque
	// constraint violation rather than something a user can act on.
	if existing, err := s.sessions.ActiveForDevice(ctx, in.DeviceID); err == nil {
		return domain.Session{}, fmt.Errorf("%w: session %s is already running on this device",
			domain.ErrConflict, existing.ID)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.Session{}, err
	}

	workingDir := in.WorkingDirectory
	if in.RepositoryID != "" {
		repo, err := s.repos.ByIDForUser(ctx, in.RepositoryID, in.UserID)
		if err != nil {
			return domain.Session{}, err
		}
		// The repository's known path wins only when the caller did not specify one.
		if workingDir == "" && repo.LocalPath != "" {
			workingDir = repo.LocalPath
		}
	}

	session := domain.Session{
		ID:               s.ids.NewID(id.PrefixSession),
		UserID:           in.UserID,
		DeviceID:         in.DeviceID,
		RepositoryID:     in.RepositoryID,
		Adapter:          in.Adapter,
		State:            protocol.StateStarting,
		WorkingDirectory: workingDir,
		StartedAt:        now,
		CreatedAt:        now,
	}
	if err := session.Validate(); err != nil {
		return domain.Session{}, err
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return domain.Session{}, err
	}

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: in.UserID, Action: domain.ActionSessionStarted,
		TargetType: "session", TargetID: session.ID,
		Metadata:  map[string]any{"device_id": in.DeviceID, "adapter": in.Adapter},
		IPAddress: in.IPAddress, CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: now,
	})
	s.log.Info("session started",
		slog.String("session_id", session.ID),
		slog.String("device_id", in.DeviceID),
		slog.String("adapter", in.Adapter))

	return session, nil
}

// Get returns one session.
func (s *SessionService) Get(ctx context.Context, sessionID, userID string) (domain.Session, error) {
	return s.sessions.ByIDForUser(ctx, sessionID, userID)
}

// List returns sessions matching a filter.
func (s *SessionService) List(ctx context.Context, f port.SessionFilter, p port.Page) ([]domain.Session, string, error) {
	return s.sessions.List(ctx, f, p)
}

// Stop requests a graceful stop.
//
// Unlike queueing a prompt, this DOES require a live connection. There is no
// sensible "stop later" semantic: a user asking to stop a session on an offline
// device should be told the device is unreachable rather than left believing the
// stop is pending.
func (s *SessionService) Stop(ctx context.Context, sessionID, userID, ipAddress string) error {
	session, err := s.sessions.ByIDForUser(ctx, sessionID, userID)
	if err != nil {
		return err
	}
	if !session.Active() {
		return fmt.Errorf("%w: session has already ended", domain.ErrConflict)
	}

	now := s.clock.Now()

	// The protocol has no STOP message: PROJECT.md fixes the set at 13 types, and
	// widening it for one control action is not worth breaking that. A NOTIFICATION
	// with a machine-readable kind carries the instruction instead, which is what
	// `kind` being a stable identifier rather than a display label is for.
	env, err := protocol.NewEnvelope(
		s.ids.NewID(id.PrefixMessage), protocol.TypeNotification, now,
		protocol.NotificationPayload{
			NotificationID: s.ids.NewID(id.PrefixNotification),
			Kind:           domain.KindSessionStopRequested,
			Title:          "Stop requested",
			Severity:       protocol.SeverityWarning,
			CreatedAt:      now,
		})
	if err != nil {
		return err
	}
	env.DeviceID = session.DeviceID
	env.SessionID = session.ID
	env.CorrelationID = blog.CorrelationIDFrom(ctx)

	if !s.conns.SendToDevice(session.DeviceID, env) {
		return domain.ErrDeviceOffline
	}

	if err := session.ApplyState(protocol.StateStopping, now); err != nil {
		return err
	}
	if err := s.sessions.Update(ctx, session); err != nil {
		return err
	}

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: userID, Action: domain.ActionSessionStopped,
		TargetType: "session", TargetID: sessionID,
		IPAddress: ipAddress, CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: now,
	})
	return nil
}

// Logs returns session output after a sequence number.
func (s *SessionService) Logs(ctx context.Context, sessionID, userID string, afterSeq int64, limit int) ([]domain.SessionLog, error) {
	// Ownership is verified through the session, so a caller cannot read another
	// user's transcript by guessing a session ID.
	if _, err := s.sessions.ByIDForUser(ctx, sessionID, userID); err != nil {
		return nil, err
	}
	return s.logs.After(ctx, sessionID, afterSeq, limit)
}

// Messages returns the conversation for a session.
func (s *SessionService) Messages(ctx context.Context, sessionID, userID string, p port.Page) ([]domain.Message, string, error) {
	if _, err := s.sessions.ByIDForUser(ctx, sessionID, userID); err != nil {
		return nil, "", err
	}
	return s.messages.ListForSession(ctx, sessionID, p)
}

// ActiveForDevice returns the running session on a device, if any.
func (s *SessionService) ActiveForDevice(ctx context.Context, deviceID string) (domain.Session, error) {
	return s.sessions.ActiveForDevice(ctx, deviceID)
}

// ApplyStatus records a STATUS frame against the session.
//
// An illegal transition is logged and dropped rather than returned as an error.
// The agent is not misbehaving: frames legitimately arrive out of order after a
// reconnect, and tearing down the connection over it would be a self-inflicted
// outage.
func (s *SessionService) ApplyStatus(ctx context.Context, sessionID string, state protocol.AgentState, currentTask string) error {
	session, err := s.sessions.ByID(ctx, sessionID)
	if err != nil {
		return err
	}
	now := s.clock.Now()

	previous := session.State
	if err := session.ApplyState(state, now); err != nil {
		s.log.Debug("ignoring out-of-order state transition",
			slog.String("session_id", sessionID),
			slog.String("from", previous.String()),
			slog.String("to", state.String()))
		return nil
	}
	if currentTask != "" {
		session.CurrentTask = currentTask
	}
	return s.sessions.Update(ctx, session)
}

// AppendLogs stores a batch of output lines.
//
// Sequence numbers continue from what is already stored, so a backend restart does
// not reset the counter and collide with existing rows, which would silently drop
// the new lines through the ON CONFLICT clause.
func (s *SessionService) AppendLogs(ctx context.Context, sessionID string, p protocol.LogPayload) error {
	if len(p.Lines) == 0 {
		return nil
	}
	maxSeq, err := s.logs.MaxSeq(ctx, sessionID)
	if err != nil {
		return err
	}

	batch := make([]domain.SessionLog, 0, len(p.Lines))
	for i, line := range p.Lines {
		batch = append(batch, domain.SessionLog{
			SessionID: sessionID,
			Seq:       maxSeq + int64(i) + 1,
			Stream:    p.Stream,
			Content:   line,
			// Only the first row of a truncated batch carries the flag, so the
			// dashboard marks the gap once rather than on every line.
			Truncated: p.Truncated && i == 0,
			At:        p.At,
			CreatedAt: s.clock.Now(),
		})
	}
	return s.logs.AppendBatch(ctx, batch)
}

// Complete ends a session after a TASK_COMPLETE frame.
func (s *SessionService) Complete(ctx context.Context, sessionID string, exitCode int, summary string) error {
	session, err := s.sessions.ByID(ctx, sessionID)
	if err != nil {
		return err
	}
	now := s.clock.Now()

	// An exit code below zero means the coding agent is still alive and merely
	// became idle, which Claude Code does between tasks. That is not the end of the
	// session, and closing it here would make the next prompt fail to find one.
	if exitCode >= 0 {
		session.End(exitCode, now)
		if err := s.sessions.Update(ctx, session); err != nil {
			return err
		}
	}

	if summary != "" {
		if err := s.messages.Create(ctx, domain.Message{
			ID:        s.ids.NewID(id.PrefixMessage),
			SessionID: sessionID,
			Role:      domain.RoleAgent,
			Content:   summary,
			CreatedAt: now,
		}); err != nil {
			s.log.Warn("failed to record completion summary", blog.Err(err))
		}
	}
	return nil
}

// SweepStale ends sessions whose device stopped reporting.
//
// Correctness rather than housekeeping: the unique partial index permits one live
// session per device, so a crashed agent's abandoned session would otherwise block
// the user from ever starting a new one on that machine.
func (s *SessionService) SweepStale(ctx context.Context, staleAfter time.Duration) (int, error) {
	cutoff := s.clock.Now().Add(-staleAfter)
	n, err := s.sessions.EndStaleSessions(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Info("ended stale sessions", slog.Int("count", n))
	}
	return n, nil
}
