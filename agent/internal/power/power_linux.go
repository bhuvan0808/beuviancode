//go:build linux

package power

import "log/slog"

// newPlatformManager returns the Linux sleep-inhibition manager.
//
// Phase 3 notes:
//
//   - Primary path is systemd-logind over D-Bus:
//     org.freedesktop.login1.Manager.Inhibit("idle:sleep", "beuvian", reason,
//     "block"), which returns a file descriptor. Sleep stays inhibited while that
//     fd is open and is released the instant it closes — including on process
//     death, so a crash cannot leak it.
//   - Fallbacks are spawning `systemd-inhibit`, or org.freedesktop.ScreenSaver on
//     older desktops.
//   - Headless servers often have none of these. The unsupported manager is the
//     correct outcome there rather than an error: a server that never sleeps needs
//     no inhibition, and refusing to run a session would be absurd.
func newPlatformManager(logger *slog.Logger) Manager {
	return &unsupportedManager{
		base: base{logger: logger, supported: false},
		goos: "linux",
	}
}
