// Command beuvian-agent is the Beuvian Desktop Agent.
//
// It supervises a local AI coding agent (Claude Code in the MVP), maintains an
// authenticated WebSocket to the Beuvian backend, streams status and output, and
// injects prompts forwarded from the dashboard.
//
// main is deliberately thin: it constructs dependencies, hands them to the
// lifecycle supervisor, and translates the result into an exit code. Everything
// else lives behind an interface in internal/.
package main

import (
	"bufio"
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
	"github.com/bhuvan0808/beuviancode/agent/internal/session"
	"github.com/bhuvan0808/beuviancode/agent/internal/store"
	"github.com/bhuvan0808/beuviancode/agent/internal/transport"
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
	// global, so it carries no cross-test state and its contents are explicit here.
	registry := coding.NewRegistry()
	if err := coding.RegisterAll(registry); err != nil {
		logger.Error("failed to register coding adapters", blog.Err(err))
		return exitRuntime
	}

	if ops.Detect {
		return runDetect(logger, registry)
	}

	stateStore := store.Open(cfg.Device.StatePath)
	state, err := stateStore.Load()
	if err != nil {
		if errors.Is(err, store.ErrCorrupt) {
			// Unrecoverable: the credentials cannot be read back. Say so plainly
			// and tell the user the one action that fixes it.
			logger.Error("the local state file could not be decrypted",
				slog.String("path", stateStore.Path()),
				slog.String("action", "delete it and re-run `beuvian-agent -register`"),
				blog.Err(err))
			return exitRuntime
		}
		logger.Error("failed to load local state", blog.Err(err))
		return exitRuntime
	}

	if ops.Register {
		return runRegister(cfg, stateStore, registry, logger)
	}

	build := version.Get()
	logger.Info("beuvian agent starting",
		slog.String("version", build.Version),
		slog.String("commit", build.Commit),
		slog.String("platform", build.Platform),
		slog.String("device_name", cfg.Device.Name),
		slog.String("adapter", cfg.Coding.Adapter),
		slog.String("backend", cfg.Backend.String()),
		slog.String("state_path", stateStore.Path()),
		slog.String("state_protection", stateStore.Protection()),
		slog.String("config_file", orNone(cfgFile)),
	)

	if lines, derr := cfg.Describe(); derr == nil {
		logger.Debug("effective configuration", slog.String("settings", strings.Join(lines, " ")))
	}

	if !registry.Has(cfg.Coding.Adapter) {
		logger.Error("the configured adapter is not registered",
			slog.String("adapter", cfg.Coding.Adapter),
			slog.Any("available", registry.Names()))
		return exitConfigError
	}
	if !coding.Implemented(cfg.Coding.Adapter) {
		logger.Error("the configured adapter is a placeholder and cannot run a session",
			slog.String("adapter", cfg.Coding.Adapter),
			slog.String("hint", "only \"claude\" is implemented"))
		return exitConfigError
	}

	if ops.Check {
		logger.Info("configuration is valid")
		return exitOK
	}

	// Registration is a precondition for everything else: the WebSocket cannot be
	// opened without a device token.
	if !state.Registered() {
		logger.Error("this device is not registered",
			slog.String("action", "run `beuvian-agent -register` with an access token from the dashboard"))
		return exitConfigError
	}
	if state.TokenExpiringSoon(time.Now().UTC()) {
		logger.Warn("the device token expires soon",
			slog.Time("expires_at", state.TokenExpiry),
			slog.String("action", "re-run `beuvian-agent -register` before it lapses"))
	}

	if err := serve(cfg, stateStore, registry, logger); err != nil {
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

// serve constructs the components and runs the supervisor.
func serve(cfg *config.Config, stateStore *store.Store, registry *coding.Registry, logger *slog.Logger) error {
	powerMgr := power.New(logger)
	if cfg.Power.Enabled {
		if st := powerMgr.Status(); !st.Supported {
			logger.Warn("sleep prevention is unavailable on this platform build",
				slog.String("impact", "the machine may sleep during a long session"))
		}
	} else {
		logger.Info("sleep prevention is disabled by configuration")
	}

	// The transport and the session manager reference each other: the manager
	// sends through the transport, and the transport dispatches inbound frames to
	// the manager. The cycle is broken once, here, rather than with an indirection
	// both sides would pay for on every call.
	client := transport.New(transport.Deps{
		Config: cfg.Backend,
		Store:  stateStore,
		Log:    logger,
		// Evaluated at each handshake rather than once at startup: a user may
		// install Claude Code while the agent is running, and the backend needs to
		// know so it can dispatch prompts to this device.
		Capabilities: func() []string {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return registry.Capabilities(ctx)
		},
	})

	manager := session.New(session.Deps{
		Config:   *cfg,
		Registry: registry,
		Sender:   client,
		Power:    powerMgr,
		Store:    stateStore,
		Log:      logger,
	})

	client.SetHandler(manager)

	sup := lifecycle.New(logger, 20*time.Second)

	// Registration order is startup order; shutdown is its exact reverse. The
	// session manager stops FIRST, so the coding agent is terminated and the sleep
	// inhibition released before the transport that reports it goes away.
	sup.Add(client)
	sup.Add(manager)

	return sup.Run(context.Background())
}

// runRegister exchanges a user access token for device credentials.
func runRegister(cfg *config.Config, stateStore *store.Store, registry *coding.Registry, logger *slog.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The token is read from stdin rather than a flag, deliberately: a command-line
	// argument lands in shell history and is visible in the process list to every
	// other user on the machine.
	fmt.Println("Beuvian device registration")
	fmt.Println()
	fmt.Printf("  1. Sign in at %s\n", cfg.Backend.APIURL)
	fmt.Println("  2. Copy your access token from the dashboard")
	fmt.Println()
	fmt.Print("Paste the access token (input is not echoed to the log): ")

	reader := bufio.NewReader(os.Stdin)
	accessToken, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "could not read the token:", err)
		return exitRuntime
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		fmt.Fprintln(os.Stderr, "no token supplied")
		return exitConfigError
	}

	capabilities := registry.Capabilities(ctx)
	if len(capabilities) == 0 {
		// Not fatal, but worth saying: a device with no coding agent installed
		// cannot service any prompt, and the user should know before they wonder
		// why nothing happens.
		fmt.Println()
		fmt.Println("Warning: no coding agents were detected on this machine.")
		fmt.Println("Install Claude Code and make sure `claude` is on your PATH.")
		fmt.Println()
	}

	resp, err := transport.NewRegistrar(cfg.Backend).Register(ctx, accessToken, capabilities)
	if err != nil {
		fmt.Fprintln(os.Stderr, "registration failed:", err)
		return exitRuntime
	}

	if err := transport.SaveRegistration(stateStore, resp, ""); err != nil {
		// The backend now holds a device we cannot use. Say exactly that, rather
		// than reporting a generic failure.
		fmt.Fprintln(os.Stderr, "registered, but the credentials could not be saved:", err)
		fmt.Fprintln(os.Stderr, "re-run registration once the problem is fixed.")
		return exitRuntime
	}

	fmt.Println()
	fmt.Println("Registered successfully.")
	fmt.Printf("  device:     %s (%s)\n", resp.Device.Name, resp.Device.ID)
	fmt.Printf("  expires:    %s\n", resp.ExpiresAt.Format(time.RFC1123))
	fmt.Printf("  state file: %s\n", stateStore.Path())
	fmt.Printf("  protection: %s\n", stateStore.Protection())
	fmt.Println()
	fmt.Println("Run `beuvian-agent` to connect.")

	logger.Info("device registered",
		slog.String("device_id", resp.Device.ID),
		slog.Any("capabilities", capabilities))
	return exitOK
}

// runDetect probes for installed coding agents and reports what it found.
func runDetect(logger *slog.Logger, registry *coding.Registry) int {
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
	_ = logger
	return exitOK
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
	// 0600: agent logs contain repository paths and task descriptions, which are
	// not for other users of a shared machine.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	// Tee rather than replace, so an interactive run still shows output while also
	// leaving a record behind.
	return io.MultiWriter(os.Stderr, f), func() { _ = f.Close() }, nil
}

func orNone(s string) string {
	if s == "" {
		return "(defaults, env and flags only)"
	}
	return s
}
