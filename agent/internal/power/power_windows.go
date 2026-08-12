//go:build windows

package power

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

// Execution state flags for SetThreadExecutionState.
const (
	esSystemRequired = 0x00000001
	esContinuous     = 0x80000000
	// esDisplayRequired is deliberately NOT used. Keeping the machine awake is
	// required; keeping the screen lit is not, and forcing the display on would
	// drain a laptop battery for no benefit to a headless coding session.
)

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

// windowsManager inhibits sleep via SetThreadExecutionState.
//
// The critical constraint: the execution state is THREAD-AFFINE. The flags apply
// to whichever OS thread made the call, and Go's scheduler moves goroutines
// between threads freely. Without pinning, the inhibition would be set on one
// thread and cleared on another — leaving the machine pinned awake with no way to
// release it. This is the single most common way this API is misused.
//
// The fix is a dedicated goroutine locked to one OS thread for the manager's whole
// lifetime, with every call funnelled to it.
type windowsManager struct {
	base

	// requests carries work to the pinned thread.
	requests chan powerRequest
	// stop shuts the pinned goroutine down.
	stop     chan struct{}
	stopOnce sync.Once
}

// powerRequest is one operation for the pinned thread to perform.
type powerRequest struct {
	flags uint32
	reply chan error
}

func newPlatformManager(logger *slog.Logger) Manager {
	m := &windowsManager{
		base:     base{logger: logger, supported: true},
		requests: make(chan powerRequest),
		stop:     make(chan struct{}),
	}
	go m.serve()
	return m
}

// serve owns the OS thread that holds the execution state.
//
// runtime.LockOSThread for the goroutine's entire life, and deliberately no
// matching UnlockOSThread: when this goroutine returns, the thread is destroyed,
// and Windows clears the execution state of a terminated thread automatically.
// That is the safety net — a crashed or exiting agent cannot leak an inhibition.
func (m *windowsManager) serve() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	for {
		select {
		case req := <-m.requests:
			ret, _, err := procSetThreadExecutionState.Call(uintptr(req.flags))
			if ret == 0 {
				req.reply <- fmt.Errorf("SetThreadExecutionState(%#x): %w", req.flags, err)
				continue
			}
			req.reply <- nil

		case <-m.stop:
			// Clear the state before releasing the thread. Belt and braces: thread
			// destruction would clear it anyway, but doing it explicitly means a
			// long-lived process that stops a session does not rely on that.
			_, _, _ = procSetThreadExecutionState.Call(uintptr(esContinuous))
			return
		}
	}
}

// call sends a request to the pinned thread and waits for the result.
func (m *windowsManager) call(flags uint32) error {
	reply := make(chan error, 1)
	select {
	case m.requests <- powerRequest{flags: flags, reply: reply}:
		return <-reply
	case <-m.stop:
		return fmt.Errorf("power: manager is shut down")
	}
}

// PreventSleep inhibits system sleep.
//
// Idempotent: a second call with a new reason updates the reason without acquiring
// a second inhibition. ES_CONTINUOUS makes the state persist until explicitly
// cleared, rather than resetting after one idle-timer period.
func (m *windowsManager) PreventSleep(reason string) error {
	if m.alreadyPrevented() {
		m.markPrevented(reason) // refresh the reason only
		return nil
	}

	if err := m.call(esContinuous | esSystemRequired); err != nil {
		return err
	}
	m.markPrevented(reason)
	m.logger.Info("system sleep inhibited",
		slog.String("reason", reason),
		slog.String("visible_via", "powercfg /requests"))
	return nil
}

// AllowSleep releases the inhibition.
//
// Safe when nothing is held, so cleanup paths need no state checks. ES_CONTINUOUS
// alone clears the previously-set requirements.
func (m *windowsManager) AllowSleep() error {
	if !m.alreadyPrevented() {
		return nil
	}
	if err := m.call(esContinuous); err != nil {
		return err
	}
	held := m.status().Held()
	m.markAllowed()
	m.logger.Info("system sleep allowed", slog.Duration("held_for", held))
	return nil
}

// Status reports the current state.
func (m *windowsManager) Status() State { return m.status() }

// Close releases the pinned thread. Part of the Closer contract in power.go.
func (m *windowsManager) Close() error {
	_ = m.AllowSleep()
	m.stopOnce.Do(func() { close(m.stop) })
	return nil
}
