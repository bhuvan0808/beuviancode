//go:build darwin

package power

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
)

// darwinManager inhibits sleep by holding a caffeinate child process.
//
// The alternative was IOPMAssertionCreateWithName, which is the "native" answer.
// caffeinate was chosen for three concrete reasons:
//
//   - IOKit requires cgo, which would end the agent's pure cross-compilation. One
//     cgo dependency turns six build targets into six build machines.
//   - `caffeinate -w <pid>` ties the inhibition to OUR process lifetime, so a
//     crashed agent releases it automatically. An IOPMAssertion handle leaked by a
//     crash keeps the machine awake until reboot.
//   - It is visible to the user in Activity Monitor and `pmset -g assertions`,
//     which matters: software that silently prevents sleep is indistinguishable
//     from a bug.
//
// The cost is a child process (a few hundred KB) and a dependency on a binary that
// has shipped with macOS since 10.8.
type darwinManager struct {
	base

	mu   sync.Mutex
	proc *exec.Cmd
}

func newPlatformManager(logger *slog.Logger) Manager {
	return &darwinManager{base: base{logger: logger, supported: true}}
}

// caffeinatePath is the standard location. Absolute rather than relying on PATH:
// the agent may run from a launch agent with a minimal environment.
const caffeinatePath = "/usr/bin/caffeinate"

// PreventSleep inhibits system sleep.
func (m *darwinManager) PreventSleep(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.proc != nil {
		m.markPrevented(reason) // already held; refresh the reason only
		return nil
	}

	if _, err := os.Stat(caffeinatePath); err != nil {
		return fmt.Errorf("%w: %s is unavailable", ErrUnsupported, caffeinatePath)
	}

	// -i inhibits idle SYSTEM sleep, not display sleep: the screen may still
	// turn off, which is what a user expects from a machine left alone.
	// -w waits on our PID, so the inhibition dies with us even on SIGKILL.
	cmd := exec.Command(caffeinatePath, "-i", "-w", fmt.Sprint(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("power: start caffeinate: %w", err)
	}

	// Reap the child so it does not become a zombie. The wait also detects
	// caffeinate being killed out from under us.
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		if m.proc == cmd {
			m.proc = nil
			if m.alreadyPrevented() {
				m.logger.Warn("caffeinate exited while a session was active; sleep is no longer inhibited")
				m.markAllowed()
			}
		}
		m.mu.Unlock()
	}()

	m.proc = cmd
	m.markPrevented(reason)
	m.logger.Info("system sleep inhibited",
		slog.String("reason", reason),
		slog.Int("caffeinate_pid", cmd.Process.Pid),
		slog.String("visible_via", "pmset -g assertions"))
	return nil
}

// AllowSleep releases the inhibition.
func (m *darwinManager) AllowSleep() error {
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
			return fmt.Errorf("power: stop caffeinate: %w", err)
		}
	}
	m.markAllowed()
	m.logger.Info("system sleep allowed", slog.Duration("held_for", held))
	return nil
}

// Status reports the current state.
func (m *darwinManager) Status() State { return m.status() }

// Close releases the inhibition.
func (m *darwinManager) Close() error { return m.AllowSleep() }
