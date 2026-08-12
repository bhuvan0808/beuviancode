package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bhuvan0808/beuviancode/agent/internal/coding"
	"github.com/bhuvan0808/beuviancode/agent/internal/config"
	"github.com/bhuvan0808/beuviancode/agent/internal/power"
	"github.com/bhuvan0808/beuviancode/agent/internal/store"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// Sender is the transport surface the manager needs.
//
// Narrowed to one method so the manager can be tested with a trivial fake rather
// than a whole WebSocket client.
type Sender interface {
	Send(env protocol.Envelope) error
	Connected() bool
	QueueDepth() int
}

// Manager coordinates the coding agent, the transport, and power management.
//
// This is the only place that knows how the pieces fit together. Everything else
// stays independent: the adapter does not know a WebSocket exists, the transport
// does not know what a coding agent is, and the power manager knows only about
// sleep. Concentrating the wiring here is what keeps those three testable in
// isolation.
type Manager struct {
	cfg      config.Config
	registry *coding.Registry
	sender   Sender
	power    power.Manager
	store    *store.Store
	log      *slog.Logger

	mu        sync.RWMutex
	adapter   coding.Adapter
	sessionID string
	state     protocol.AgentState
	startedAt time.Time

	// logBuf accumulates output between flushes. Batching is not an optimisation:
	// a verbose build emits thousands of lines per second, and one frame each
	// would saturate the socket and the database.
	logBuf       []string
	logStream    protocol.LogStream
	logFirstAt   time.Time
	logTruncated bool
	logMu        sync.Mutex

	stopOnce sync.Once
	stopped  chan struct{}
	wg       sync.WaitGroup
}

// Deps groups the manager's collaborators.
type Deps struct {
	Config   config.Config
	Registry *coding.Registry
	Sender   Sender
	Power    power.Manager
	Store    *store.Store
	Log      *slog.Logger
}

// New builds a Manager.
func New(d Deps) *Manager {
	return &Manager{
		cfg:      d.Config,
		registry: d.Registry,
		sender:   d.Sender,
		power:    d.Power,
		store:    d.Store,
		log:      d.Log.With(slog.String("component", "session")),
		state:    protocol.StateIdle,
		stopped:  make(chan struct{}),
	}
}

// Name identifies the component in lifecycle logs.
func (m *Manager) Name() string { return "session" }

// Start begins the background loops. Non-blocking.
func (m *Manager) Start(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)

	m.wg.Add(2)
	go func() { defer m.wg.Done(); m.statusLoop(ctx) }()
	go func() { defer m.wg.Done(); m.flushLoop(ctx) }()

	if m.cfg.Coding.AutoStart && m.cfg.Coding.WorkingDirectory != "" {
		go func() {
			// Wait for the transport, so the STATUS frames from startup are not
			// buffered through a disconnected socket and delivered out of order.
			time.Sleep(2 * time.Second)
			if err := m.StartSession(ctx, m.cfg.Coding.WorkingDirectory, ""); err != nil {
				m.log.Error("auto-start failed", blog.Err(err))
			}
		}()
	}
	return nil
}

// Stop ends any session and releases the sleep inhibition.
func (m *Manager) Stop(ctx context.Context) error {
	m.stopOnce.Do(func() { close(m.stopped) })

	if err := m.StopSession(ctx); err != nil {
		m.log.Warn("failed to stop the coding session cleanly", blog.Err(err))
	}

	// Unconditional release. AllowSleep is safe when nothing is held, and a leaked
	// inhibition would drain the user's battery indefinitely — the worst possible
	// parting gift from a background agent.
	if err := power.Release(m.power); err != nil {
		m.log.Warn("failed to release sleep inhibition", blog.Err(err))
	}

	m.wg.Wait()
	return nil
}

// StartSession launches the coding agent in a directory.
func (m *Manager) StartSession(ctx context.Context, workingDir, initialPrompt string) error {
	m.mu.Lock()
	if m.adapter != nil && m.state.Active() {
		m.mu.Unlock()
		return coding.ErrAlreadyRunning
	}
	m.mu.Unlock()

	adapter, err := m.registry.New(m.cfg.Coding.Adapter, m.log)
	if err != nil {
		return err
	}
	if !coding.Implemented(m.cfg.Coding.Adapter) {
		return fmt.Errorf("%w: %s", coding.ErrNotImplemented, m.cfg.Coding.Adapter)
	}

	sessionID := id.WithPrefix(id.PrefixSession)

	// Inhibit sleep BEFORE launching. A machine that sleeps between the launch and
	// the inhibition would strand the session in exactly the window we care about.
	if m.cfg.Power.Enabled {
		if err := m.power.PreventSleep("Beuvian is running a coding session"); err != nil {
			// Not fatal: an unsupported platform should still run sessions. The
			// user is told their machine may sleep.
			if errors.Is(err, power.ErrUnsupported) {
				m.log.Warn("sleep cannot be prevented on this platform; the session may be interrupted")
			} else {
				m.log.Warn("failed to inhibit sleep", blog.Err(err))
			}
		}
	}

	if err := adapter.Start(ctx, coding.StartOptions{
		WorkingDirectory: workingDir,
		ExecutablePath:   m.cfg.Coding.ExecutablePath,
		Args:             m.cfg.Coding.Args,
		InitialPrompt:    initialPrompt,
	}); err != nil {
		// Release the inhibition we just took: a failed start must not leave the
		// machine pinned awake.
		if m.cfg.Power.Enabled {
			_ = m.power.AllowSleep()
		}
		return err
	}

	m.mu.Lock()
	m.adapter = adapter
	m.sessionID = sessionID
	m.state = protocol.StateStarting
	m.startedAt = time.Now()
	m.mu.Unlock()

	if err := m.store.Update(func(st *store.State) { st.LastSessionID = sessionID }); err != nil {
		m.log.Warn("failed to persist the session id", blog.Err(err))
	}

	m.log.Info("session started",
		slog.String("session_id", sessionID),
		slog.String("working_directory", workingDir),
		slog.String("adapter", m.cfg.Coding.Adapter))

	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.consumeOutput(ctx, adapter, sessionID) }()

	m.sendStatus(ctx)
	return nil
}

// StopSession terminates the coding agent.
func (m *Manager) StopSession(ctx context.Context) error {
	m.mu.Lock()
	adapter := m.adapter
	m.mu.Unlock()

	if adapter == nil {
		return nil
	}

	// A bounded stop, so a wedged coding agent cannot hang the agent's shutdown.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	err := adapter.Stop(stopCtx)

	if m.cfg.Power.Enabled {
		if aerr := m.power.AllowSleep(); aerr != nil {
			m.log.Warn("failed to release sleep inhibition", blog.Err(aerr))
		}
	}

	m.mu.Lock()
	m.adapter = nil
	m.state = protocol.StateStopped
	m.mu.Unlock()

	return err
}

// consumeOutput drains the adapter's output and detects completion.
func (m *Manager) consumeOutput(ctx context.Context, adapter coding.Adapter, sessionID string) {
	for line := range adapter.ReadOutput() {
		m.appendLog(line)
	}

	// The channel closed, so the process exited. Flush anything buffered before
	// reporting completion, or the final output — the part explaining why it
	// exited — would be lost.
	m.flush(ctx)

	exitCode, _ := adapter.ExitCode()
	status := adapter.Status()

	m.mu.Lock()
	stillCurrent := m.sessionID == sessionID
	if stillCurrent {
		m.state = status.State
	}
	m.mu.Unlock()

	if !stillCurrent {
		return // superseded by a newer session
	}

	// Release the inhibition: the session is over, and holding it would keep the
	// user's machine awake for nothing.
	if m.cfg.Power.Enabled {
		if err := m.power.AllowSleep(); err != nil {
			m.log.Warn("failed to release sleep inhibition", blog.Err(err))
		}
	}

	m.sendTaskComplete(ctx, sessionID, exitCode, status)
	m.sendStatus(ctx)

	m.log.Info("session ended",
		slog.String("session_id", sessionID),
		slog.Int("exit_code", exitCode),
		slog.String("state", status.State.String()))
}

// appendLog buffers one output line.
func (m *Manager) appendLog(line coding.OutputLine) {
	m.logMu.Lock()
	defer m.logMu.Unlock()

	// Truncate a pathological single line (a minified bundle, a base64 blob)
	// rather than letting it breach the protocol's frame limit.
	text := line.Text
	if len(text) > m.cfg.Session.MaxLogLineBytes {
		text = text[:m.cfg.Session.MaxLogLineBytes] + "... [truncated]"
		m.logTruncated = true
	}

	if len(m.logBuf) == 0 {
		m.logFirstAt = line.At
		m.logStream = line.Stream
	}
	// A stream change forces a flush: stdout and stderr cannot share a batch,
	// since the payload carries one stream for the whole batch.
	if line.Stream != m.logStream {
		m.flushLocked(context.Background())
		m.logFirstAt = line.At
		m.logStream = line.Stream
	}

	m.logBuf = append(m.logBuf, text)

	if len(m.logBuf) >= m.cfg.Session.LogBatchSize {
		m.flushLocked(context.Background())
	}
}

// flushLoop sends buffered output on a timer.
func (m *Manager) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Session.LogFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopped:
			m.flush(ctx)
			return
		case <-ctx.Done():
			m.flush(ctx)
			return
		case <-ticker.C:
			m.flush(ctx)
		}
	}
}

func (m *Manager) flush(ctx context.Context) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	m.flushLocked(ctx)
}

// flushLocked sends the buffered lines. Caller must hold logMu.
func (m *Manager) flushLocked(_ context.Context) {
	if len(m.logBuf) == 0 {
		return
	}

	m.mu.RLock()
	sessionID := m.sessionID
	m.mu.RUnlock()

	if sessionID == "" {
		// Output with no session has nowhere to go. Drop it rather than
		// accumulating forever.
		m.logBuf = m.logBuf[:0]
		m.logTruncated = false
		return
	}

	lines := make([]string, len(m.logBuf))
	copy(lines, m.logBuf)
	m.logBuf = m.logBuf[:0]
	truncated := m.logTruncated
	m.logTruncated = false

	env, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypeLog, time.Now().UTC(),
		protocol.LogPayload{
			Stream:    m.logStream,
			Lines:     lines,
			At:        m.logFirstAt,
			Truncated: truncated,
		})
	if err != nil {
		return
	}
	env.SessionID = sessionID

	if err := m.sender.Send(env); err != nil {
		m.log.Debug("failed to queue log batch", blog.Err(err))
	}
}

// statusLoop sends STATUS periodically and performs idle detection.
func (m *Manager) statusLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Session.StatusInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopped:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.detectIdle(ctx)
			// STATUS is sent on the timer as well as on transitions, so the
			// dashboard converges to the truth even if a transition frame was lost.
			m.sendStatus(ctx)
		}
	}
}

// detectIdle infers that the coding agent is waiting for input.
//
// This is a heuristic, and the most consequential one in the product. Claude Code
// emits no machine-readable "I need you" signal, so idleness is inferred from
// output falling silent.
//
// The two failure modes are NOT symmetric. A premature notification is mildly
// annoying. A missed one means the user believes work is progressing when it
// stopped forty minutes ago — the exact failure Beuvian exists to prevent. So this
// leans toward notifying.
func (m *Manager) detectIdle(ctx context.Context) {
	m.mu.RLock()
	adapter := m.adapter
	sessionID := m.sessionID
	state := m.state
	m.mu.RUnlock()

	if adapter == nil || sessionID == "" || state != protocol.StateRunning {
		return
	}

	// Only the real adapter tracks output timing; a placeholder cannot.
	timer, ok := adapter.(interface {
		LastOutputAt() time.Time
		MarkWaiting()
	})
	if !ok {
		return
	}

	idleFor := time.Since(timer.LastOutputAt())
	if idleFor < m.cfg.Session.IdleTimeout {
		return
	}

	timer.MarkWaiting()

	m.mu.Lock()
	m.state = protocol.StateWaitingInput
	m.mu.Unlock()

	m.log.Info("coding agent appears to be waiting for input",
		slog.Duration("idle_for", idleFor),
		slog.String("session_id", sessionID))

	env, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypeTaskWaiting, time.Now().UTC(),
		protocol.TaskWaitingPayload{
			Reason:     protocol.WaitPrompt,
			DetectedAt: time.Now().UTC(),
		})
	if err != nil {
		return
	}
	env.SessionID = sessionID
	if err := m.sender.Send(env); err != nil {
		m.log.Debug("failed to queue TASK_WAITING", blog.Err(err))
	}

	m.sendStatus(ctx)
}

// sendStatus reports the current state to the backend.
func (m *Manager) sendStatus(_ context.Context) {
	m.mu.RLock()
	adapter := m.adapter
	sessionID := m.sessionID
	state := m.state
	startedAt := m.startedAt
	m.mu.RUnlock()

	payload := protocol.StatusPayload{
		State:         state,
		Adapter:       m.cfg.Coding.Adapter,
		QueuedPrompts: m.pendingCount(),
	}
	if adapter != nil {
		st := adapter.Status()
		payload.State = st.State
		payload.CPUPercent = st.CPUPercent
		payload.MemoryBytes = st.MemoryBytes
		payload.PID = st.PID
		payload.Repository = adapter.Repository()
		payload.WorkingDirectory = adapter.WorkingDirectory()
		payload.CurrentTask = adapter.CurrentTask()
	}
	if !startedAt.IsZero() {
		// A duration rather than a start timestamp, so the dashboard need not
		// trust this machine's wall clock.
		payload.ElapsedSeconds = int64(time.Since(startedAt).Seconds())
	}

	env, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypeStatus, time.Now().UTC(), payload)
	if err != nil {
		return
	}
	env.SessionID = sessionID

	if err := m.sender.Send(env); err != nil {
		m.log.Debug("failed to queue STATUS", blog.Err(err))
	}
}

// sendTaskComplete reports that the coding agent finished.
func (m *Manager) sendTaskComplete(_ context.Context, sessionID string, exitCode int, status coding.Status) {
	env, err := protocol.NewEnvelope(
		id.WithPrefix(id.PrefixMessage), protocol.TypeTaskComplete, time.Now().UTC(),
		protocol.TaskCompletePayload{
			ExitCode:        exitCode,
			DurationSeconds: int64(status.Elapsed().Seconds()),
			Summary:         m.recentOutput(),
		})
	if err != nil {
		return
	}
	env.SessionID = sessionID
	if err := m.sender.Send(env); err != nil {
		m.log.Debug("failed to queue TASK_COMPLETE", blog.Err(err))
	}
}

// recentOutput returns a short tail of output for the completion notification.
func (m *Manager) recentOutput() string {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	if len(m.logBuf) == 0 {
		return ""
	}
	last := m.logBuf[len(m.logBuf)-1]
	const maxSummary = 300
	if len(last) > maxSummary {
		return last[:maxSummary] + "..."
	}
	return last
}

// pendingCount reports the offline prompt queue depth.
func (m *Manager) pendingCount() int {
	return len(m.store.Current().PendingPrompts)
}

// State returns the current session state, for diagnostics.
func (m *Manager) State() (protocol.AgentState, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state, m.sessionID
}
