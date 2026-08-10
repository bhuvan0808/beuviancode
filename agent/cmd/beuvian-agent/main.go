// Command beuvian-agent is the Beuvian Desktop Agent.
//
// It supervises a local AI coding agent (Claude Code in the MVP), maintains an
// authenticated WebSocket to the Beuvian backend, streams status and output, and
// injects prompts forwarded from the dashboard.
//
// Phase 1 scope: configuration, logging, the coding-adapter registry, the power
// manager, and supervised startup/shutdown. The session manager and WebSocket
// transport arrive in Phase 3 as additional lifecycle components; this bootstrap
// does not change when they do.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bhuvan0808/beuviancode/agent/internal/coding"
	"github.com/bhuvan0808/beuviancode/agent/internal/config"
	"github.com/bhuvan0808/beuviancode/agent/internal/power"
	"github.com/bhuvan0808/beuviancode/shared/lifecycle"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/version"
)

const (
	exitOK          = 0
	exitRuntime     = 1
	exitConfigError = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, ops, cfgFile, err := config.Load(args)
	if err != nil {
		if errors.Is(err, config.ErrHelp) {
			return exitOK
		}
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return exitConfigError
	}

	if ops.Version {
		fmt.Println("beuvian-agent", version.Short())
		return exitOK
	}

	logWriter, closeLog, err := openLogWriter(cfg.Log.FilePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open log file:", err)
		return exitConfigError
	}
	defer closeLog()

	logger := blog.New(blog.Config{
		Level:     cfg.Log.Level,
		Format:    blog.Format(cfg.Log.Format),
		AddSource: cfg.Log.AddSource,
		Component: "agent",
	}, logWriter)

	// The adapter registry is constructed per-process rather than as a package
	// global, so it carries no cross-test state and its contents are explicit
	// here. Phase 3 adds the real Claude adapter alongside these placeholders.
	registry := coding.NewRegistry()
	if err := coding.RegisterPlaceholders(registry); err != nil {
		logger.Error("failed to register coding adapters", blog.Err(err))
		return exitRuntime
	}

	// -detect is the first diagnostic to reach for when a user reports that
	// Beuvian cannot find their coding agent, so it works without a valid
	// connection or device registration.
	if ops.Detect {
		return runDetect(logger, registry)
	}

	build := version.Get()
	logger.Info("beuvian agent starting",
		slog.String("version", build.Version),
		slog.String("commit", build.Commit),
		slog.String("platform", build.Platform),
		slog.String("device_name", cfg.Device.Name),
		slog.String("adapter", cfg.Coding.Adapter),
		slog.String("backend", cfg.Backend.String()),
		slog.String("state_path", cfg.Device.StatePath),
		slog.String("config_file", orNone(cfgFile)),
	)

	if lines, derr := cfg.Describe(); derr == nil {
		logger.Debug("effective configuration", slog.String("settings", strings.Join(lines, " ")))
	}

	// Fail early on an adapter name that does not exist. Discovering this at the
	// moment a user tries to start a session — after they have walked away from
	// the laptop — is exactly the wrong time.
	if !registry.Has(cfg.Coding.Adapter) {
		logger.Error("configured adapter is not registered",
			slog.String("adapter", cfg.Coding.Adapter),
			slog.Any("available", registry.Names()))
		return exitConfigError
	}
	if !coding.Implemented(cfg.Coding.Adapter) {
		logger.Warn("selected adapter is a placeholder and cannot run a session yet",
			slog.String("adapter", cfg.Coding.Adapter),
			slog.String("note", "adapter implementations land in phase 3"))
	}

	if ops.Check {
		logger.Info("configuration is valid")
		return exitOK
	}

	sup := lifecycle.New(logger, 10*time.Second)

	// Power management is registered first so it is released last: sleep must
	// stay inhibited until the coding session has actually finished shutting
	// down, not while it is still draining.
	if cfg.Power.Enabled {
		sup.Add(newPowerComponent(power.New(logger), logger))
	} else {
		logger.Info("sleep prevention is disabled by configuration")
	}

	// Phase 3 registers the real work here, in dependency order:
	//   sup.Add(store.New(cfg.Device.StatePath, logger))
	//   sup.Add(transport.NewClient(cfg.Backend, state, logger))
	//   sup.Add(session.NewManager(cfg, registry, powerMgr, transport, logger))
	sup.Add(lifecycle.Func{
		ComponentName: "bootstrap",
		OnStart: func(context.Context) error {
			logger.Warn("no session components registered yet",
				slog.String("phase", "1"),
				slog.String("note", "session manager and WebSocket transport arrive in phase 3"))
			return nil
		},
	})

	if err := sup.Run(context.Background()); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("stopped")
			return exitOK
		}
		logger.Error("terminated with errors", blog.Err(err))
		return exitRuntime
	}

	logger.Info("stopped cleanly")
	return exitOK
}

// runDetect probes for installed coding agents and reports what it found.
func runDetect(logger *slog.Logger, registry *coding.Registry) int {
	// Bounded so one hanging `--version` call cannot make the diagnostic itself
	// look broken.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	found, problems := registry.DetectAll(ctx)

	fmt.Println("Registered adapters:", strings.Join(registry.Names(), ", "))
	fmt.Println()

	if len(found) == 0 {
		fmt.Println("No coding agents were found on PATH.")
	} else {
		names := make([]string, 0, len(found))
		for name := range found {
			names = append(names, name)
		}
		sort.Strings(names)

		fmt.Println("Installed:")
		for _, name := range names {
			inst := found[name]
			note := "  (detected; adapter not implemented yet)"
			if coding.Implemented(name) {
				note = ""
			}
			fmt.Printf("  %-10s %s%s\n", name, inst.ExecutablePath, note)
			if inst.Version != "" {
				fmt.Printf("  %-10s %s\n", "", inst.Version)
			}
		}
	}

	// Detector failures are reported rather than hidden: a broken detector looks
	// identical to an uninstalled tool unless we say so.
	if len(problems) > 0 {
		fmt.Println()
		fmt.Println("Detection problems:")
		for name, err := range problems {
			fmt.Printf("  %-10s %v\n", name, err)
		}
	}

	if _, ok := found[coding.AdapterClaude]; !ok {
		fmt.Println()
		fmt.Println("Claude Code was not found. Beuvian does not install or replace it —")
		fmt.Println("install it yourself and make sure `claude` is on your PATH.")
		return exitRuntime
	}
	return exitOK
}

// newPowerComponent wraps the power manager as a lifecycle component.
//
// Acquiring the inhibition at startup rather than per-session is a Phase 1
// simplification with a deliberate boundary: Phase 3's session manager takes
// ownership and holds it only while a session is active, which is what PROJECT.md
// specifies. Until then the agent holds nothing, because the current
// implementation is the honest unsupported one.
func newPowerComponent(mgr power.Manager, logger *slog.Logger) lifecycle.Component {
	return lifecycle.Func{
		ComponentName: "power",
		OnStart: func(context.Context) error {
			st := mgr.Status()
			if !st.Supported {
				// Not an error: the agent is perfectly usable, the user just
				// needs to know their machine may sleep mid-session.
				logger.Warn("sleep prevention is unavailable on this platform build",
					slog.String("note", "the machine may sleep during a long session"))
			}
			return nil
		},
		OnStop: func(context.Context) error {
			// Unconditional release. AllowSleep is safe when nothing is held, and
			// a leaked inhibition would drain the user's battery indefinitely —
			// the worst possible parting gift from a background agent.
			return mgr.AllowSleep()
		},
	}
}

// openLogWriter returns the log sink, duplicating to a file when configured.
//
// Desktop software needs a file sink: when a user reports a problem, whatever was
// on stdout is long gone.
func openLogWriter(path string) (io.Writer, func(), error) {
	if path == "" {
		return os.Stderr, func() {}, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	// 0600: agent logs can contain repository paths and task descriptions, which
	// are not for other users of a shared machine.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	// Tee rather than replace, so an interactive run still shows output while
	// also leaving a record behind.
	return io.MultiWriter(os.Stderr, f), func() { _ = f.Close() }, nil
}

func orNone(s string) string {
	if s == "" {
		return "(defaults, env and flags only)"
	}
	return s
}
