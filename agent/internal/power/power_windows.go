//go:build windows

package power

import "log/slog"

// newPlatformManager returns the Windows sleep-inhibition manager.
//
// Phase 3 will implement this via SetThreadExecutionState from kernel32. The
// design decisions are recorded here so they are not re-litigated later:
//
//   - The call is SetThreadExecutionState(ES_CONTINUOUS|ES_SYSTEM_REQUIRED).
//     ES_DISPLAY_REQUIRED is deliberately excluded: keeping the machine awake is
//     required, keeping the screen lit is not, and forcing the display on would
//     drain a laptop battery for no benefit.
//   - The flags are thread-affine. The goroutine issuing the call must be pinned
//     with runtime.LockOSThread, or the scheduler may migrate it and the
//     inhibition will be released by a thread that never acquired it. This is the
//     most common way this API is misused.
//   - Release is SetThreadExecutionState(ES_CONTINUOUS) from that same thread.
//   - Windows drops the state when the process exits, which is the safety net
//     for a crash: a killed agent cannot leak an inhibition.
//   - It will use golang.org/x/sys/windows rather than syscall, which is frozen.
//
// Until then the unsupported manager is returned, so the agent runs and warns
// honestly rather than claiming an inhibition it is not holding.
func newPlatformManager(logger *slog.Logger) Manager {
	return &unsupportedManager{
		base: base{logger: logger, supported: false},
		goos: "windows",
	}
}
