package coding

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// Adapter names. Constants rather than bare strings because they appear in
// configuration files, the AUTH capability list, and the dashboard; a typo in one
// place should be a compile error, not a silently unavailable adapter.
const (
	AdapterClaude    = "claude"
	AdapterCodex     = "codex"
	AdapterGemini    = "gemini"
	AdapterAider     = "aider"
	AdapterOpenHands = "openhands"
)

// placeholder is a registered-but-unimplemented adapter.
//
// PROJECT.md requires placeholder adapters for Codex CLI, Gemini CLI, Aider, and
// OpenHands, explicitly without implementing them. A placeholder is better than
// an absent entry for two reasons: it proves the Adapter interface is genuinely
// sufficient for tools other than Claude (if a placeholder could not satisfy the
// interface, the abstraction would be Claude-shaped and the extension point a
// fiction), and it makes the eventual implementation a matter of replacing one
// file rather than adding wiring across the codebase.
//
// Every method fails with ErrNotImplemented instead of returning a zero value, so
// an accidental selection surfaces immediately rather than presenting as a coding
// agent that silently does nothing.
type placeholder struct {
	name string
	// executables are the command names Detect looks for on PATH. Detection is
	// implemented even for placeholders, because the dashboard can then
	// truthfully show "Aider is installed but not yet supported" — which is more
	// useful than showing nothing.
	executables []string
	logger      *slog.Logger
}

// newPlaceholder returns a Factory for an unimplemented adapter.
func newPlaceholder(name string, executables ...string) Factory {
	return func(logger *slog.Logger) Adapter {
		if logger == nil {
			logger = slog.Default()
		}
		return &placeholder{
			name:        name,
			executables: executables,
			logger:      logger.With(slog.String("adapter", name)),
		}
	}
}

func (p *placeholder) Name() string { return p.name }

func (p *placeholder) Start(context.Context, StartOptions) error {
	p.logger.Error("adapter selected but not implemented",
		slog.String("hint", "set agent.adapter to \"claude\""))
	return fmt.Errorf("%w: %s", ErrNotImplemented, p.name)
}

func (p *placeholder) Stop(context.Context) error { return nil } // nothing runs; Stop is trivially idempotent

func (p *placeholder) Status() Status {
	return Status{State: protocol.StateIdle}
}

func (p *placeholder) SendPrompt(context.Context, string) error {
	return fmt.Errorf("%w: %s", ErrNotImplemented, p.name)
}

// ReadOutput returns a closed channel so a consumer's range loop terminates
// immediately rather than blocking forever on a nil channel.
func (p *placeholder) ReadOutput() <-chan OutputLine {
	ch := make(chan OutputLine)
	close(ch)
	return ch
}

func (p *placeholder) CurrentTask() string      { return "" }
func (p *placeholder) Repository() string       { return "" }
func (p *placeholder) WorkingDirectory() string { return "" }
func (p *placeholder) ExitCode() (int, bool)    { return 0, false }

// Detect looks for the tool on PATH. Implemented even though the adapter is not,
// so device capability reporting is accurate.
func (p *placeholder) Detect(ctx context.Context) (Installation, error) {
	return detectOnPath(ctx, p.executables...)
}

// detectOnPath locates the first of names present on PATH and reads its version.
//
// Shared by the placeholders and available to the real Claude adapter in Phase 3,
// so detection logic exists once.
func detectOnPath(ctx context.Context, names ...string) (Installation, error) {
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		return Installation{
			ExecutablePath: path,
			Version:        readVersion(ctx, path),
			DetectedAt:     time.Now().UTC(),
		}, nil
	}
	return Installation{}, fmt.Errorf("%w: none of %v found on PATH", ErrNotInstalled, names)
}

// readVersion best-effort reads `<tool> --version`.
//
// A failure yields "" rather than an error: not knowing the version must never
// block using a tool that is demonstrably present.
func readVersion(ctx context.Context, path string) string {
	// A short timeout because this runs during detection sweeps; a tool that
	// hangs on --version must not stall device registration.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	// Take the first line only: several CLIs print a banner after the version.
	line := strings.TrimSpace(string(out))
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// placeholderDetector adapts a placeholder to the Detector interface.
type placeholderDetector struct {
	name        string
	executables []string
}

func (d placeholderDetector) Name() string { return d.name }

func (d placeholderDetector) Detect(ctx context.Context) (Installation, error) {
	return detectOnPath(ctx, d.executables...)
}

// Implemented reports whether name has a working adapter implementation.
//
// Detection and implementation are deliberately separate concerns: Beuvian can
// truthfully tell a user "Claude Code is installed at C:\...\claude.cmd" in
// Phase 1, before any adapter can drive it. Conflating the two would mean either
// lying about what is installed or hiding installations we can already see.
//
// This is the single place Phase 3 edits when ClaudeAdapter lands.
func Implemented(name string) bool {
	implemented := []string{
		// Phase 3 adds AdapterClaude here.
	}
	for _, n := range implemented {
		if n == name {
			return true
		}
	}
	return false
}

// knownAdapters maps every adapter name to the executables that indicate it is
// installed. Detection works for all of them, including those not yet driveable.
func knownAdapters() []struct {
	name        string
	executables []string
} {
	return []struct {
		name        string
		executables []string
	}{
		// Claude Code is the MVP target. On Windows the npm installer produces a
		// claude.cmd shim rather than a .exe, and LookPath only finds it if the
		// name is tried explicitly — omitting it makes Beuvian report "not
		// installed" on the exact platform the MVP ships for.
		{AdapterClaude, []string{"claude", "claude.cmd", "claude.exe"}},
		{AdapterCodex, []string{"codex", "codex.cmd"}},
		{AdapterGemini, []string{"gemini", "gemini.cmd"}},
		{AdapterAider, []string{"aider", "aider.exe"}},
		{AdapterOpenHands, []string{"openhands", "opendevin"}},
	}
}

// RegisterPlaceholders registers every adapter named in PROJECT.md.
//
// In Phase 1 all of them are placeholders whose Start fails with
// ErrNotImplemented, but each carries a real Detector, so `beuvian-agent -detect`
// gives an accurate picture of the machine. Phase 3 replaces the Claude factory
// with the real adapter; nothing else changes, which is the extension point
// working as designed.
func RegisterPlaceholders(r *Registry) error {
	for _, a := range knownAdapters() {
		err := r.Register(a.name,
			newPlaceholder(a.name, a.executables...),
			placeholderDetector{name: a.name, executables: a.executables})
		if err != nil {
			return err
		}
	}
	return nil
}
