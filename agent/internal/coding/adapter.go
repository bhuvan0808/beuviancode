// Package coding defines the pluggable interface over AI coding agents.
//
// This is the extension point PROJECT.md is built around: Claude Code ships in
// the MVP, and Codex CLI, Gemini CLI, Aider, and OpenHands must be addable
// "with minimal code changes". That is achieved by depending on the Adapter
// interface everywhere and constructing concrete adapters only through the
// registry, so nothing outside this package knows which coding agent is running.
//
// Phase 1 defines the contract. Phase 3 implements ClaudeAdapter against it.
package coding

import (
	"context"
	"errors"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// Adapter supervises one AI coding agent process.
//
// The method set is PROJECT.md's, adjusted in two ways that matter for a
// production implementation:
//
//   - Methods that perform I/O take a context, so a hung coding agent cannot
//     block shutdown forever. A Stop() with no deadline is the difference
//     between a clean exit and a wedged process on the user's machine.
//   - ReadOutput returns a channel rather than a byte slice. Output is an
//     unbounded stream arriving at unpredictable rates; a pull-based reader
//     would either block the caller or silently drop lines.
//
// Implementations must be safe for concurrent use: the session manager reads
// status while the WebSocket loop delivers prompts.
type Adapter interface {
	// Name is the stable adapter identifier used in configuration, in the
	// AUTH handshake's capability list, and by the dashboard: "claude".
	Name() string

	// Start launches the coding agent. It returns once the process is spawned,
	// not once it is idle and ready — readiness is observed through Status
	// transitions, because only the adapter knows what readiness looks like for
	// its particular tool.
	Start(ctx context.Context, opts StartOptions) error

	// Stop terminates the coding agent, attempting a graceful exit first and
	// escalating to a kill when ctx expires. Stop must be idempotent: the
	// session manager may call it during a crash recovery that already exited.
	Stop(ctx context.Context) error

	// Status returns a snapshot of the current state. Must not block: it is
	// called on the heartbeat path.
	Status() Status

	// SendPrompt injects a prompt into the coding agent's stdin.
	//
	// Returns ErrNotAcceptingInput when the agent is not in a state that can
	// receive one. The caller re-queues rather than discarding, which is what
	// makes a prompt sent from a phone survive a coding agent that is mid-task.
	SendPrompt(ctx context.Context, prompt string) error

	// ReadOutput returns the stream of output lines.
	//
	// The channel is closed when the process exits, which is how the session
	// manager learns the stream has ended without polling. Exactly one consumer
	// is expected; the session manager owns it and fans out from there.
	ReadOutput() <-chan OutputLine

	// CurrentTask describes what the coding agent is working on, best-effort.
	// Empty when unknown — the adapter must not invent a task description.
	CurrentTask() string

	// Repository is the git repository in the working directory, in
	// "owner/name" form when it can be determined, else "".
	Repository() string

	// WorkingDirectory is the absolute path the coding agent runs in.
	WorkingDirectory() string

	// ExitCode returns the process exit code. The bool is false while the
	// process is still running, which distinguishes "exited with 0" from "has
	// not exited" — a distinction a bare int cannot express.
	ExitCode() (int, bool)
}

// Detector reports whether a coding agent is installed.
//
// Separate from Adapter because detection happens before any process exists, and
// because the dashboard needs to show what is available on a device without
// starting anything.
type Detector interface {
	// Name matches the corresponding Adapter's Name.
	Name() string

	// Detect locates the installation. It returns ErrNotInstalled when the tool
	// is absent, which is an ordinary outcome and not a failure.
	Detect(ctx context.Context) (Installation, error)
}

// Installation describes a located coding agent.
type Installation struct {
	// ExecutablePath is the absolute path to the binary or launcher.
	ExecutablePath string
	// Version is the reported version string, or "" if it could not be read.
	Version string
	// DetectedAt records when detection ran, so a stale result can be refreshed.
	DetectedAt time.Time
}

// StartOptions configures a launch.
type StartOptions struct {
	// WorkingDirectory is where the coding agent runs. Required: defaulting it
	// to the agent's own working directory would silently run against the wrong
	// repository, which is a destructive mistake.
	WorkingDirectory string

	// ExecutablePath overrides detection. Empty means "detect".
	ExecutablePath string

	// Args are extra command-line arguments passed through verbatim.
	Args []string

	// Env holds additional environment variables as "KEY=VALUE".
	//
	// Appended to the inherited environment rather than replacing it: the coding
	// agent needs the user's PATH and its own credentials, which Beuvian
	// deliberately never handles (PROJECT.md: users keep paying for their own
	// agents, and Beuvian never sees provider API keys).
	Env []string

	// InitialPrompt is sent once the agent is ready, if non-empty.
	InitialPrompt string
}

// Status is a point-in-time view of the supervised process.
type Status struct {
	State protocol.AgentState

	// PID is the OS process ID, or 0 when nothing is running.
	PID int

	// StartedAt is when the process launched; zero when not running.
	StartedAt time.Time

	// CPUPercent and MemoryBytes feed the dashboard's resource readouts.
	CPUPercent  float64
	MemoryBytes uint64

	// LastOutputAt is when output was last observed. Idle detection is derived
	// from it: a quiet process is how "waiting for input" is inferred for tools
	// that give no explicit signal.
	LastOutputAt time.Time

	// Err holds the failure that moved the adapter into StateCrashed.
	Err error
}

// Elapsed returns how long the current session has been running.
func (s Status) Elapsed() time.Duration {
	if s.StartedAt.IsZero() {
		return 0
	}
	return time.Since(s.StartedAt)
}

// OutputLine is one line of output from the coding agent.
type OutputLine struct {
	Stream protocol.LogStream
	Text   string
	At     time.Time
}

// Sentinel errors. Callers branch on these, so they are part of the package's
// contract and are compared with errors.Is rather than by string matching.
var (
	// ErrNotInstalled means the coding agent could not be found on this machine.
	ErrNotInstalled = errors.New("coding: agent is not installed")

	// ErrAlreadyRunning means Start was called on a running adapter.
	ErrAlreadyRunning = errors.New("coding: agent is already running")

	// ErrNotRunning means an operation requires a running process.
	ErrNotRunning = errors.New("coding: agent is not running")

	// ErrNotAcceptingInput means the agent cannot take a prompt right now. The
	// caller should re-queue rather than discard.
	ErrNotAcceptingInput = errors.New("coding: agent is not accepting input")

	// ErrUnsupportedAdapter means no adapter is registered under that name.
	ErrUnsupportedAdapter = errors.New("coding: unsupported adapter")

	// ErrNotImplemented marks a placeholder adapter. PROJECT.md requires
	// placeholders for Codex, Gemini, Aider, and OpenHands without implementing
	// them; returning this is how they announce that honestly instead of
	// appearing to work.
	ErrNotImplemented = errors.New("coding: adapter is not implemented yet")
)
