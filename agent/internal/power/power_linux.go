//go:build linux

package power

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
)

// linuxManager inhibits sleep via systemd-inhibit.
//
// The alternative was speaking D-Bus to org.freedesktop.login1 directly, which is
// the "proper" answer and returns an inhibitor file descriptor. systemd-inhibit
// was chosen because it gives the same guarantee with none of the complexity: it
// holds the fd on our behalf and releases it when the child exits, including if we
// are SIGKILLed. A leaked D-Bus inhibitor fd, by contrast, survives until the
// owning connection closes.
//
// Headless servers frequently have no logind at all. That is not an error: a
// machine that never sleeps needs no inhibition, and refusing to run a coding
// session there would be absurd. The manager degrades to unsupported and says so.
type linuxManager struct {
	base

	mu   sync.Mutex
	proc *exec.Cmd
}

func newPlatformManager(logger *slog.Logger) Manager {
	supported := systemdInhibitPath() != ""
	if !supported {
		// Headless or non-systemd. Report honestly rather than pretending.
		return &unsupportedManager{
			base: base{logger: logger, supported: false},
			goos: "linux (systemd-inhibit not found)",
		}
	}
	return &linuxManager{base: base{logger: logger, supported: true}}
}

// systemdInhibitPath locates systemd-inhibit, or returns "".
func systemdInhibitPath() string {
	// Absolute paths first: the agent may run from a systemd unit with a minimal
	// PATH that does not include /usr/bin.
	for _, candidate := range []string{"/usr/bin/systemd-inhibit", "/bin/systemd-inhibit"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if path, err := exec.LookPath("systemd-inhibit"); err == nil {
		return path
	}
	return ""
}

// PreventSleep inhibits system sleep.
func (m *linuxManager) PreventSleep(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.proc != nil {
		m.markPrevented(reason)
		return nil
	}

	inhibit := systemdInhibitPath()
	if inhibit == "" {
		return fmt.Errorf("%w: systemd-inhibit is unavailable", ErrUnsupported)
	}

	// --what=idle:sleep blocks idle-triggered and explicit sleep, but NOT
	// shutdown: a user shutting their machine down should never be blocked by a
	// background agent.
	//
	// The inhibited command is a `sleep` that outlives any session; killing the
	// wrapper releases the lock. --mode=block makes it a hard inhibitor rather
	// than a delay.
	cmd := exec.Command(inhibit,
		"--what=idle:sleep",
		"--who=Beuvian",
		"--why="+reason,
		"--mode=block",
		"sleep", "infinity",
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("power: start systemd-inhibit: %w", err)
	}

	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		if m.proc == cmd {
			m.proc = nil
			if m.alreadyPrevented() {
				m.logger.Warn("systemd-inhibit exited while a session was active; sleep is no longer inhibited")
				m.markAllowed()
			}
		}
		m.mu.Unlock()
	}()

	m.proc = cmd
	m.markPrevented(reason)
	m.logger.Info("system sleep inhibited",
		slog.String("reason", reason),
		slog.Int("inhibit_pid", cmd.Process.Pid),
		slog.String("visible_via", "systemd-inhibit --list"))
	return nil
}

// AllowSleep releases the inhibition.
func (m *linuxManager) AllowSleep() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.proc == nil {
		m.markAllowed()
		return nil
	}

	held := m.status().Held()
	proc := m.proc
	m.proc = nil

	if proc.Process != nil {
		if err := proc.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("power: stop systemd-inhibit: %w", err)
		}
	}
	m.markAllowed()
	m.logger.Info("system sleep allowed", slog.Duration("held_for", held))
	return nil
}

// Status reports the current state.
func (m *linuxManager) Status() State { return m.status() }

// Close releases the inhibition.
func (m *linuxManager) Close() error { return m.AllowSleep() }
