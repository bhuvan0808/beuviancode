package coding

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// claudeAdapter supervises a Claude Code process.
//
// The design constraint that shapes everything here: Claude Code is an
// interactive terminal program, not an API. Beuvian drives it by writing to its
// stdin and reading its stdout, exactly as a human at a keyboard would. There is
// no structured protocol to rely on, so every inference (is it working? is it
// waiting?) is made from output timing rather than from an explicit signal.
type claudeAdapter struct {
	logger *slog.Logger

	// mu guards everything below. The session manager reads Status while the
	// output reader writes to it, so this is genuinely concurrent.
	mu sync.RWMutex

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc

	state     protocol.AgentState
	startedAt time.Time
	pid       int

	// exited and exitCode together express what a bare int cannot: whether the
	// process has finished at all.
	exited   bool
	exitCode int
	lastErr  error

	workingDir  string
	repository  string
	currentTask string

	lastOutputAt time.Time

	// output is the stream handed to the session manager. Buffered so a brief
	// stall in the consumer does not block the process's stdout pipe, which would
	// eventually make Claude Code itself block on a full pipe buffer.
	output chan OutputLine

	// closeOutput guards against double-closing the channel when Stop races with
	// the process exiting on its own.
	closeOutput sync.Once
}

// newClaudeAdapter returns an adapter for Claude Code.
func newClaudeAdapter(logger *slog.Logger) Adapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &claudeAdapter{
		logger: logger.With(slog.String("adapter", AdapterClaude)),
		state:  protocol.StateIdle,
	}
}

// outputBuffer is the adapter's stdout/stderr channel depth.
//
// Sized for a burst: a verbose build emits thousands of lines in a second, and a
// smaller buffer would make the reader goroutine block on the consumer, which
// back-pressures into the OS pipe and eventually stalls Claude Code itself.
const outputBuffer = 4096

func (a *claudeAdapter) Name() string { return AdapterClaude }

// Start launches Claude Code in the given working directory.
func (a *claudeAdapter) Start(ctx context.Context, opts StartOptions) error {
	a.mu.Lock()
	if a.state.Active() {
		a.mu.Unlock()
		return ErrAlreadyRunning
	}
	a.mu.Unlock()

	if opts.WorkingDirectory == "" {
		// No default is possible. Claude Code writes files, so running in the
		// wrong directory destroys real work.
		return fmt.Errorf("%w: working directory is required", ErrNotAcceptingInput)
	}
	info, err := os.Stat(opts.WorkingDirectory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("coding: working directory %q is not usable: %w", opts.WorkingDirectory, err)
	}

	executable := opts.ExecutablePath
	if executable == "" {
		inst, derr := detectOnPath(ctx, claudeExecutables()...)
		if derr != nil {
			return derr
		}
		executable = inst.ExecutablePath
	}

	// The process gets its own cancellable context, detached from the caller's:
	// Start's ctx may be a short-lived request context, and cancelling it must not
	// kill a 45-minute coding session.
	procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	cmd := exec.CommandContext(procCtx, executable, opts.Args...)
	cmd.Dir = opts.WorkingDirectory
	// Append to the inherited environment rather than replacing it. Claude Code
	// needs the user's PATH and its OWN credentials — which Beuvian deliberately
	// never handles, reads, or forwards.
	cmd.Env = append(os.Environ(), opts.Env...)
	configureProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("coding: open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("coding: open stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("coding: open stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("coding: launch %s: %w", executable, err)
	}

	now := time.Now()
	a.mu.Lock()
	a.cmd = cmd
	a.stdin = stdin
	a.cancel = cancel
	a.state = protocol.StateStarting
	a.startedAt = now
	a.lastOutputAt = now
	a.pid = cmd.Process.Pid
	a.exited = false
	a.exitCode = 0
	a.lastErr = nil
	a.workingDir = opts.WorkingDirectory
	a.repository = detectRepository(opts.WorkingDirectory)
	a.currentTask = ""
	a.output = make(chan OutputLine, outputBuffer)
	a.closeOutput = sync.Once{}
	out := a.output
	a.mu.Unlock()

	a.logger.Info("claude code started",
		slog.String("executable", executable),
		slog.String("working_directory", opts.WorkingDirectory),
		slog.String("repository", a.repository),
		slog.Int("pid", cmd.Process.Pid))

	// One reader per stream. They must not share a goroutine: a blocking read on
	// stdout would starve stderr, and a crash message on stderr is exactly what is
	// needed when stdout has gone quiet.
	var streams sync.WaitGroup
	streams.Add(2)
	go func() { defer streams.Done(); a.readStream(stdout, protocol.StreamStdout, out) }()
	go func() { defer streams.Done(); a.readStream(stderr, protocol.StreamStderr, out) }()

	// Reap the process and close the output channel once both readers have
	// finished, so a consumer ranging over the channel sees every line before it
	// observes the exit.
	go a.reap(cmd, &streams, out)

	if opts.InitialPrompt != "" {
		// Give the process a moment to reach its prompt. Writing immediately is
		// unreliable: an interactive program that has not finished initialising
		// discards stdin, so the instruction would silently vanish.
		go func() {
			time.Sleep(2 * time.Second)
			if err := a.SendPrompt(procCtx, opts.InitialPrompt); err != nil {
				a.logger.Warn("failed to send the initial prompt", slog.String("error", err.Error()))
			}
		}()
	}

	return nil
}

// readStream forwards one output stream into the channel.
func (a *claudeAdapter) readStream(r io.Reader, stream protocol.LogStream, out chan<- OutputLine) {
	scanner := bufio.NewScanner(r)
	// Claude Code prints long lines (diffs, file contents). The default 64 KiB
	// scanner limit would return an error mid-session and silently truncate the
	// transcript, so raise it to the protocol's own frame limit.
	scanner.Buffer(make([]byte, 0, 64*1024), protocol.MaxMessageBytes)

	for scanner.Scan() {
		line := scanner.Text()

		now := time.Now()
		a.mu.Lock()
		a.lastOutputAt = now
		// The first output means the process is past initialisation and genuinely
		// running. Inferred rather than assumed at Start: a binary that fails
		// immediately would otherwise look "running" until it exited.
		if a.state == protocol.StateStarting {
			a.state = protocol.StateRunning
		} else if a.state == protocol.StateWaitingInput {
			// Output after an idle period means it picked the work back up.
			a.state = protocol.StateRunning
		}
		a.mu.Unlock()

		select {
		case out <- OutputLine{Stream: stream, Text: line, At: now}:
		default:
			// The consumer is too far behind. Dropping the line is better than
			// blocking: back-pressure here propagates into the OS pipe and
			// eventually stalls Claude Code itself, turning a monitoring problem
			// into a work stoppage. The session manager marks the batch truncated.
			a.logger.Warn("output buffer full; dropping a line",
				slog.String("stream", string(stream)))
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		a.logger.Debug("output stream ended",
			slog.String("stream", string(stream)), slog.String("error", err.Error()))
	}
}

// reap waits for the process and records how it ended.
func (a *claudeAdapter) reap(cmd *exec.Cmd, streams *sync.WaitGroup, out chan OutputLine) {
	// Wait for the readers first: cmd.Wait closes the pipes, and closing them
	// before the readers drain would truncate the final output — which is
	// precisely the output explaining why the process exited.
	streams.Wait()

	err := cmd.Wait()

	a.mu.Lock()
	a.exited = true
	a.exitCode = cmd.ProcessState.ExitCode()
	a.pid = 0

	switch {
	case a.state == protocol.StateStopping:
		// We asked it to stop, so this is a clean exit regardless of the code.
		a.state = protocol.StateStopped
	case err == nil && a.exitCode == 0:
		a.state = protocol.StateStopped
	default:
		a.state = protocol.StateCrashed
		a.lastErr = err
	}
	finalState, code := a.state, a.exitCode
	a.mu.Unlock()

	a.logger.Info("claude code exited",
		slog.Int("exit_code", code), slog.String("state", finalState.String()))

	// Closing the channel is how the session manager learns the stream ended,
	// without polling.
	a.closeOutput.Do(func() { close(out) })
}

// Stop terminates Claude Code, escalating if it does not exit.
//
// Idempotent: crash-recovery paths call it on adapters that already exited.
func (a *claudeAdapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	if a.cmd == nil || a.cmd.Process == nil || a.exited {
		a.mu.Unlock()
		return nil
	}
	a.state = protocol.StateStopping
	proc := a.cmd.Process
	stdin := a.stdin
	cancel := a.cancel
	a.mu.Unlock()

	a.logger.Info("stopping claude code", slog.Int("pid", proc.Pid))

	// Closing stdin is the gentlest exit for an interactive program: it reads EOF
	// and shuts down on its own terms, flushing whatever it was writing.
	if stdin != nil {
		_ = stdin.Close()
	}

	// Then a polite signal. On Windows there is no SIGTERM, so terminateGracefully
	// does the platform-appropriate thing.
	if err := terminateGracefully(proc); err != nil {
		a.logger.Debug("graceful termination signal failed", slog.String("error", err.Error()))
	}

	// Wait for the exit, bounded by the caller's deadline. Without a deadline a
	// wedged process would hang shutdown indefinitely.
	deadline := time.After(stopGrace(ctx))
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.mu.RLock()
			done := a.exited
			a.mu.RUnlock()
			if done {
				return nil
			}
		case <-deadline:
			// Escalate. Cancelling the context kills the process group, so a
			// coding agent that spawned children does not leave orphans behind.
			a.logger.Warn("claude code did not exit gracefully; killing it")
			cancel()
			_ = killProcessGroup(proc)
			return nil
		case <-ctx.Done():
			cancel()
			_ = killProcessGroup(proc)
			return ctx.Err()
		}
	}
}

// stopGrace returns how long to wait for a graceful exit.
func stopGrace(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		// Leave a slice of the budget for the kill path, so escalation happens
		// inside the caller's deadline rather than after it.
		if remaining := time.Until(deadline) - time.Second; remaining > 0 {
			return remaining
		}
		return 0
	}
	return 10 * time.Second
}

// Status returns a snapshot. Must not block: it is on the heartbeat path.
func (a *claudeAdapter) Status() Status {
	a.mu.RLock()
	defer a.mu.RUnlock()

	st := Status{
		State:        a.state,
		PID:          a.pid,
		StartedAt:    a.startedAt,
		LastOutputAt: a.lastOutputAt,
		Err:          a.lastErr,
	}
	if usage, err := processUsage(a.pid); err == nil {
		st.CPUPercent = usage.cpuPercent
		st.MemoryBytes = usage.memoryBytes
	}
	return st
}

// SendPrompt writes a prompt to Claude Code's stdin.
func (a *claudeAdapter) SendPrompt(_ context.Context, prompt string) error {
	a.mu.Lock()
	stdin := a.stdin
	state := a.state
	exited := a.exited
	a.mu.Unlock()

	if stdin == nil || exited {
		return ErrNotRunning
	}
	// StateStarting means it has produced no output yet and is probably still
	// initialising. Writing now would be discarded, so the caller re-queues
	// instead — which is why this returns ErrNotAcceptingInput rather than
	// swallowing the prompt.
	if state == protocol.StateStarting {
		return fmt.Errorf("%w: still starting up", ErrNotAcceptingInput)
	}
	if state.Terminal() {
		return ErrNotRunning
	}

	// A trailing newline is what submits the input, exactly as pressing Enter
	// would. Without it the text sits in Claude Code's input buffer and nothing
	// happens, which looks identical to a delivery failure.
	text := strings.TrimRight(prompt, "\r\n") + "\n"

	if _, err := io.WriteString(stdin, text); err != nil {
		// A broken pipe means the process died between the state check and the
		// write. Report it as not-running so the caller re-queues rather than
		// treating the prompt as delivered.
		if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			return ErrNotRunning
		}
		return fmt.Errorf("coding: write prompt: %w", err)
	}

	now := time.Now()
	a.mu.Lock()
	a.lastOutputAt = now // an injected prompt counts as activity for idle detection
	a.currentTask = firstLine(prompt)
	if a.state == protocol.StateWaitingInput {
		a.state = protocol.StateRunning
	}
	a.mu.Unlock()

	a.logger.Info("prompt injected", slog.Int("bytes", len(text)))
	return nil
}

// ReadOutput returns the output stream. Closed when the process exits.
func (a *claudeAdapter) ReadOutput() <-chan OutputLine {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.output == nil {
		// A closed channel rather than nil: a nil channel blocks a range loop
		// forever, whereas a closed one ends it immediately.
		ch := make(chan OutputLine)
		close(ch)
		return ch
	}
	return a.output
}

// MarkWaiting records that the session manager inferred an idle state.
//
// The adapter cannot make this call itself: idleness is a timing judgement that
// depends on configuration the adapter does not hold. Keeping the inference in the
// session manager and the state here means one owner for the value and one owner
// for the policy.
func (a *claudeAdapter) MarkWaiting() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == protocol.StateRunning {
		a.state = protocol.StateWaitingInput
	}
}

// LastOutputAt reports when output was last seen, for idle detection.
func (a *claudeAdapter) LastOutputAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastOutputAt
}

func (a *claudeAdapter) CurrentTask() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.currentTask
}

func (a *claudeAdapter) Repository() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.repository
}

func (a *claudeAdapter) WorkingDirectory() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.workingDir
}

func (a *claudeAdapter) ExitCode() (int, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.exitCode, a.exited
}

// Detect locates Claude Code.
func (a *claudeAdapter) Detect(ctx context.Context) (Installation, error) {
	return detectOnPath(ctx, claudeExecutables()...)
}

// claudeExecutables lists the names Claude Code may be installed under.
//
// claude.cmd matters specifically: the npm installer produces a .cmd shim on
// Windows rather than a .exe, and exec.LookPath only finds it when the name is
// tried explicitly. Omitting it would make Beuvian report "not installed" on the
// exact platform the MVP targets.
func claudeExecutables() []string {
	return []string{"claude", "claude.cmd", "claude.exe"}
}

// firstLine returns a short single-line summary of a prompt, for CurrentTask.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const maxTask = 120
	if len(s) > maxTask {
		return s[:maxTask] + "..."
	}
	return s
}

// detectRepository resolves the git repository in dir, best-effort.
//
// Reads .git/config directly rather than shelling out to git: the agent runs on a
// user's machine where git may be absent, and spawning a process on every status
// snapshot would be wasteful. An unresolvable repository is not an error — the
// session simply reports none.
func detectRepository(dir string) string {
	for cur := dir; ; {
		configPath := filepath.Join(cur, ".git", "config")
		if body, err := os.ReadFile(configPath); err == nil {
			if name := parseRemoteFullName(string(body)); name != "" {
				return name
			}
			return "" // a git repo with no usable origin
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "" // reached the filesystem root
		}
		cur = parent
	}
}

// parseRemoteFullName extracts "owner/name" from a git config's origin URL.
func parseRemoteFullName(config string) string {
	lines := strings.Split(config, "\n")
	inOrigin := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[remote ") {
			inOrigin = strings.Contains(trimmed, `"origin"`)
			continue
		}
		if !inOrigin || !strings.HasPrefix(trimmed, "url") {
			continue
		}
		_, rawURL, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		return normaliseGitURL(strings.TrimSpace(rawURL))
	}
	return ""
}

// normaliseGitURL reduces the several git URL forms to "owner/name".
func normaliseGitURL(raw string) string {
	raw = strings.TrimSuffix(raw, ".git")

	// scp-like: git@github.com:owner/name
	if _, after, ok := strings.Cut(raw, ":"); ok && strings.Contains(raw, "@") && !strings.Contains(raw, "//") {
		return trimToLastTwoSegments(after)
	}
	// https://github.com/owner/name or ssh://git@github.com/owner/name
	if _, after, ok := strings.Cut(raw, "://"); ok {
		if _, path, found := strings.Cut(after, "/"); found {
			return trimToLastTwoSegments(path)
		}
	}
	return trimToLastTwoSegments(raw)
}

// trimToLastTwoSegments keeps the final owner/name pair of a path.
func trimToLastTwoSegments(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}
