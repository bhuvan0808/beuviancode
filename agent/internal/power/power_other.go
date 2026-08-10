//go:build !windows && !darwin && !linux

package power

import (
	"log/slog"
	"runtime"
)

// newPlatformManager is the fallback for platforms Beuvian does not target
// explicitly (FreeBSD, OpenBSD, illumos, and anything future).
//
// Present so the module compiles for every GOOS the Go toolchain supports. A
// missing implementation would otherwise turn `GOOS=freebsd go build ./...` into
// an undefined-symbol error, which is a confusing way to learn that a platform is
// merely untested rather than broken.
func newPlatformManager(logger *slog.Logger) Manager {
	return &unsupportedManager{
		base: base{logger: logger, supported: false},
		goos: runtime.GOOS,
	}
}
