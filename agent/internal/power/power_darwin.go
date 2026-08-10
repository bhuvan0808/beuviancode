//go:build darwin

package power

import "log/slog"

// newPlatformManager returns the macOS sleep-inhibition manager.
//
// Phase 3 notes:
//
//   - The native approach is IOPMAssertionCreateWithName with
//     kIOPMAssertPreventUserIdleSystemSleep, released via IOPMAssertionRelease.
//     Named assertions show up in `pmset -g assertions`, so the user can see
//     precisely what is holding their machine awake.
//   - That requires cgo and the IOKit framework, which conflicts with producing
//     statically cross-compiled release binaries from CI.
//   - Favoured alternative: spawn `/usr/bin/caffeinate -i -w <pid>`. No cgo, the
//     inhibition is tied to our process lifetime so a crash releases it
//     automatically, and it is visible to the user in Activity Monitor.
//   - Final choice is deferred to Phase 3, when the release pipeline's cgo
//     constraints are settled.
func newPlatformManager(logger *slog.Logger) Manager {
	return &unsupportedManager{
		base: base{logger: logger, supported: false},
		goos: "darwin",
	}
}
