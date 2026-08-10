// Command server is the Beuvian backend API and WebSocket gateway.
//
// Phase 1 scope: this binary establishes the process contract — configuration
// resolution, structured logging, and supervised startup/shutdown — and then
// blocks until signalled. Phase 2 registers the Fiber HTTP server, the database
// pool, the Redis client, and the WebSocket gateway as lifecycle components; the
// bootstrap below does not change when they arrive, only the Add calls.
//
// main is deliberately thin. Everything it does is wiring: construct
// dependencies, hand them to the supervisor, translate the result into an exit
// code. Business logic lives in internal/app, reachable through interfaces in
// internal/port, so it is testable without a process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/shared/lifecycle"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/version"
)

func main() {
	// A single exit point. os.Exit skips deferred functions, so calling it from
	// anywhere but here would silently bypass cleanup.
	os.Exit(run(os.Args[1:]))
}

// Exit codes are distinct so an orchestrator or CI step can tell a
// misconfiguration (which redeploying will not fix) from a runtime fault.
const (
	exitOK          = 0
	exitConfigError = 2
	exitRuntime     = 1
)

func run(args []string) int {
	cfg, ops, cfgFile, err := config.Load(args)
	if err != nil {
		if errors.Is(err, config.ErrHelp) {
			return exitOK // a help request is a success, not a failure
		}
		// Config errors go to stderr in plain text: the logger is configured
		// *from* the config, so it does not exist yet at this point.
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return exitConfigError
	}

	// -version works even where configuration is incomplete, so it is handled
	// before anything else is constructed.
	if ops.Version {
		fmt.Println("beuvian-backend", version.Short())
		return exitOK
	}

	logger := blog.New(blog.Config{
		Level:     cfg.Log.Level,
		Format:    blog.Format(cfg.Log.Format),
		AddSource: cfg.Log.AddSource,
		Component: "backend",
	}, os.Stdout)

	build := version.Get()
	logger.Info("beuvian backend starting",
		slog.String("version", build.Version),
		slog.String("commit", build.Commit),
		slog.String("go", build.GoVersion),
		slog.String("platform", build.Platform),
		slog.String("env", string(cfg.Env)),
		slog.String("addr", cfg.Server.Addr()),
		slog.String("config_file", orNone(cfgFile)),
	)

	// Log the effective configuration once, secrets redacted. This is the
	// fastest possible answer to "which settings is this instance actually
	// running with?", which is the first question in almost every incident.
	if lines, derr := cfg.Describe(); derr == nil {
		logger.Debug("effective configuration", slog.String("settings", strings.Join(lines, " ")))
	}

	warnOnDevelopmentShortcuts(logger, cfg)

	// -check validates configuration and exits. Used by CI and by operators
	// verifying a deploy's environment before it takes traffic.
	if ops.Check {
		logger.Info("configuration is valid")
		return exitOK
	}

	sup := lifecycle.New(logger, cfg.Server.ShutdownGrace)

	// Phase 2 registers real components here, in dependency order, e.g.:
	//   sup.Add(postgres.New(cfg.Database, logger))
	//   sup.Add(redis.New(cfg.Redis, logger))
	//   sup.Add(ws.NewGateway(cfg.WebSocket, hub, logger))
	//   sup.Add(http.NewServer(cfg.Server, router, logger))
	// Registration order is startup order; shutdown is its reverse, so the HTTP
	// server drains before the pools it depends on close.
	sup.Add(lifecycle.Func{
		ComponentName: "bootstrap",
		OnStart: func(context.Context) error {
			logger.Warn("no service components registered yet",
				slog.String("phase", "1"),
				slog.String("note", "HTTP, database, Redis and WebSocket arrive in phase 2"))
			return nil
		},
	})

	if err := sup.Run(context.Background()); err != nil {
		if errors.Is(err, context.Canceled) {
			// A cancelled parent context is an ordinary stop, not a fault.
			logger.Info("stopped")
			return exitOK
		}
		logger.Error("terminated with errors", blog.Err(err))
		return exitRuntime
	}

	logger.Info("stopped cleanly")
	return exitOK
}

// warnOnDevelopmentShortcuts surfaces settings that are tolerable locally but
// would be defects elsewhere.
//
// Validate() rejects these outright outside development; warning about them in
// development means the eventual production failure is not a surprise.
func warnOnDevelopmentShortcuts(logger *slog.Logger, cfg *config.Config) {
	if !cfg.Env.IsDevelopment() {
		return
	}
	if cfg.Auth.JWTSecret == "" {
		logger.Warn("auth.jwt_secret is empty; tokens cannot be issued (development only)")
	}
	if cfg.Database.URL == "" {
		logger.Warn("database.url is empty; persistence is unavailable (development only)")
	}
	if cfg.Redis.URL == "" {
		logger.Warn("redis.url is empty; prompt dispatch falls back to database polling (development only)")
	}
	if cfg.RateLimit.Enabled && cfg.Redis.URL == "" {
		logger.Warn("rate limiting is enabled but Redis is absent; requests are NOT being limited")
	}
}

func orNone(s string) string {
	if s == "" {
		return "(defaults, env and flags only)"
	}
	return s
}
