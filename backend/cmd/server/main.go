// Command server is the Beuvian backend API and WebSocket gateway.
//
// main is deliberately thin. Everything here is wiring: construct dependencies,
// hand them to the lifecycle supervisor, translate the result into an exit code.
// Business logic lives in internal/app, reachable through interfaces in
// internal/port, so it is testable without starting a process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/bhuvan0808/beuviancode/backend/internal/adapter/auth"
	beuvianhttp "github.com/bhuvan0808/beuviancode/backend/internal/adapter/http"
	"github.com/bhuvan0808/beuviancode/backend/internal/adapter/postgres"
	beuvianredis "github.com/bhuvan0808/beuviancode/backend/internal/adapter/redis"
	"github.com/bhuvan0808/beuviancode/backend/internal/adapter/ws"
	"github.com/bhuvan0808/beuviancode/backend/internal/app"
	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/lifecycle"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
	"github.com/bhuvan0808/beuviancode/shared/version"
)

func main() {
	// A single exit point. os.Exit skips deferred functions, so calling it from
	// anywhere else would silently bypass cleanup.
	os.Exit(run(os.Args[1:]))
}

// Exit codes are distinct so an orchestrator or CI step can tell a
// misconfiguration (which redeploying will not fix) from a runtime fault.
const (
	exitOK          = 0
	exitRuntime     = 1
	exitConfigError = 2
)

func run(args []string) int {
	cfg, ops, cfgFile, err := config.Load(args)
	if err != nil {
		if errors.Is(err, config.ErrHelp) {
			return exitOK // a help request is a success, not a failure
		}
		// Plain stderr: the logger is configured FROM the config, so it does not
		// exist yet at this point.
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return exitConfigError
	}

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

	// The effective configuration, secrets redacted, in one line. This is the
	// fastest answer to "which settings is this instance actually running with?",
	// which is the first question in almost every incident.
	if lines, derr := cfg.Describe(); derr == nil {
		logger.Debug("effective configuration", slog.String("settings", strings.Join(lines, " ")))
	}

	warnOnDevelopmentShortcuts(logger, cfg)

	if ops.Check {
		logger.Info("configuration is valid")
		return exitOK
	}
	if ops.Migrate {
		return runMigrations(cfg, logger)
	}

	if err := serve(cfg, logger); err != nil {
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

// serve constructs every component and runs the supervisor.
func serve(cfg *config.Config, logger *slog.Logger) error {
	// --- Infrastructure components -------------------------------------------
	db := postgres.New(cfg.Database, logger)
	rdb := beuvianredis.New(cfg.Redis, logger)

	sup := lifecycle.New(logger, cfg.Server.ShutdownGrace)

	// Registration order is startup order; shutdown is its exact reverse. That
	// ordering is what makes deploys safe: the HTTP server stops accepting
	// requests before the pools it depends on are closed, so in-flight requests
	// finish rather than failing on every deploy.
	sup.Add(db, rdb)

	// The remaining components need the pools, which only exist after Start. A
	// deferred component builds them at start time, once its dependencies are up.
	var (
		hub     = ws.NewHub(logger)
		clock   = postgres.SystemClock{}
		ids     = postgres.IDGen{}
		httpSrv *beuvianhttp.Server

		// stopBackground tears down the pub/sub and maintenance goroutines.
		//
		// Captured here rather than registering more components from inside
		// OnStart: the supervisor has already iterated its component list by then,
		// so anything added would never be started and — worse — never stopped.
		stopBackground = func() {}
	)

	sup.Add(lifecycle.Func{
		ComponentName: "services",
		OnStart: func(ctx context.Context) error {
			pool := db.Pool()

			// --- Stores (adapters implementing the port interfaces) ---
			users := postgres.NewUserStore(pool)
			devices := postgres.NewDeviceStore(pool)
			repos := postgres.NewRepositoryStore(pool)
			sessions := postgres.NewSessionStore(pool)
			sessionLogs := postgres.NewSessionLogStore(pool)
			messages := postgres.NewMessageStore(pool)
			queue := postgres.NewPromptQueueStore(pool)
			notifications := postgres.NewNotificationStore(pool)
			refreshTokens := postgres.NewRefreshTokenStore(pool)
			audit := postgres.NewAuditStore(pool, logger)

			// --- Redis-backed infrastructure ---
			presence := beuvianredis.NewPresence(rdb)
			dispatcher := beuvianredis.NewDispatcher(rdb, logger)
			events := beuvianredis.NewEvents(rdb, logger)
			limiter := beuvianredis.NewLimiter(rdb)
			locks := beuvianredis.NewLock(rdb, logger)
			cache := beuvianredis.NewCache(rdb)

			// --- Auth ---
			tokens := auth.NewTokenService(cfg.Auth)
			github := auth.NewGitHub(cfg.Auth)

			// --- Use cases ---
			authSvc := app.NewAuthService(app.AuthDeps{
				Users: users, Tokens: refreshTokens, Issuer: tokens, Verifier: tokens,
				OAuth: github, Cache: cache, Audit: audit, IDs: ids, Clock: clock,
				Log: logger, StateTTL: cfg.Auth.StateTTL,
			})
			deviceSvc := app.NewDeviceService(app.DeviceDeps{
				Devices: devices, Sessions: sessions, Prompts: queue,
				Presence: presence, Conns: hub, Issuer: tokens, Verifier: tokens,
				Audit: audit, IDs: ids, Clock: clock, Log: logger,
			})
			sessionSvc := app.NewSessionService(app.SessionDeps{
				Sessions: sessions, Logs: sessionLogs, Messages: messages,
				Devices: devices, Repos: repos, Conns: hub, Events: events,
				Audit: audit, IDs: ids, Clock: clock, Log: logger,
			})
			promptSvc := app.NewPromptService(app.PromptDeps{
				Prompts: queue, Devices: devices, Sessions: sessions, Messages: messages,
				Dispatcher: dispatcher, Conns: hub, Audit: audit,
				IDs: ids, Clock: clock, Log: logger,
			})
			notifySvc := app.NewNotificationService(app.NotificationDeps{
				Notifications: notifications, Users: users,
				// The dashboard is the MVP's only channel. WhatsApp, Telegram,
				// Slack, Discord, and push each become another entry here with no
				// change to the notification use case (PROJECT.md extension point).
				Channels: []port.NotificationChannel{
					app.NewDashboardChannel(events, ids, clock),
				},
				IDs: ids, Clock: clock, Log: logger,
			})
			repoSvc := app.NewRepositoryService(app.RepositoryDeps{
				Repos: repos, Users: users, OAuth: github, Cache: cache,
				IDs: ids, Clock: clock, Log: logger,
			})
			settingsSvc := app.NewSettingsService(users, audit, clock)

			// --- WebSocket gateway ---
			gateway := ws.NewGateway(ws.GatewayDeps{
				Hub: hub, Config: cfg.WebSocket, Auth: authSvc, Devices: deviceSvc,
				Sessions: sessionSvc, Prompts: promptSvc, Notifs: notifySvc,
				Cache: cache, Events: events, Clock: clock, Log: logger,
			})

			// --- HTTP server ---
			httpSrv = beuvianhttp.New(beuvianhttp.Deps{
				Config: beuvianhttp.Config{
					Server: cfg.Server, Auth: cfg.Auth, CORS: cfg.CORS,
					RateLimit: cfg.RateLimit, Env: cfg.Env,
				},
				Auth: authSvc, Devices: deviceSvc, Sessions: sessionSvc,
				Prompts: promptSvc, Repos: repoSvc, Notifs: notifySvc,
				Settings: settingsSvc, Limiter: limiter, Conns: hub, Clock: clock,
				Health: []beuvianhttp.HealthCheck{
					{
						Name:     "database",
						Critical: true, // no database means the API cannot serve
						Check:    func(c *fiber.Ctx) error { return db.Health(c.UserContext()) },
					},
					{
						Name: "redis",
						// Non-critical when Redis is optional: the backend genuinely
						// still works (prompts stay durable in PostgreSQL), and
						// reporting unhealthy would pull a serving instance out of
						// rotation over a recoverable degradation.
						Critical: cfg.Redis.Required,
						Check:    func(c *fiber.Ctx) error { return rdb.Health(c.UserContext()) },
					},
				},
				WebSocketHandler: gateway.Handler(),
				Log:              logger,
			})

			// Cross-instance fan-out. Both subscriptions run for the process
			// lifetime; without them, an agent on instance A is unreachable from an
			// API call served by instance B, which silently breaks prompt delivery
			// at any scale above one instance.
			stopSubs := startSubscriptions(dispatcher, events, promptSvc, hub, logger)

			// Periodic maintenance, guarded by a distributed lock so only one
			// instance performs each sweep.
			stopJobs := startMaintenance(locks, sessionSvc, promptSvc, sessionLogs, refreshTokens, logger)

			stopBackground = func() {
				stopSubs()
				stopJobs()
			}
			return nil
		},
		OnStop: func(context.Context) error {
			stopBackground()
			// Close sockets before the HTTP server stops, so clients see a clean
			// close frame rather than a dropped TCP connection.
			hub.CloseAll()
			return nil
		},
	})

	// The HTTP listener is registered last, so it is the FIRST thing stopped.
	sup.Add(lifecycle.Func{
		ComponentName: "httpserver",
		OnStart: func(ctx context.Context) error {
			if httpSrv == nil {
				return errors.New("http server was not constructed")
			}
			return httpSrv.Start(ctx)
		},
		OnStop: func(ctx context.Context) error {
			if httpSrv == nil {
				return nil
			}
			return httpSrv.Stop(ctx)
		},
	})

	return sup.Run(context.Background())
}

// startSubscriptions runs the Redis pub/sub loops and returns a stop function.
func startSubscriptions(
	dispatcher port.PromptDispatcher,
	events port.EventPublisher,
	prompts *app.PromptService,
	hub *ws.Hub,
	logger *slog.Logger,
) func() {
	// Detached from the request context on purpose: these loops live for the
	// process, not for whatever call happened to construct them.
	subCtx, cancel := context.WithCancel(context.Background())

	go func() {
		err := dispatcher.Subscribe(subCtx, func(deviceID, promptID string) {
			// Only act if the device is connected HERE. Every instance receives the
			// broadcast; the one holding the socket is the one that delivers.
			if !hub.DeviceConnected(deviceID) {
				return
			}
			if err := prompts.DeliverByID(subCtx, deviceID, promptID); err != nil {
				logger.Warn("dispatch delivery failed",
					slog.String("device_id", deviceID), blog.Err(err))
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("prompt dispatch subscription ended", blog.Err(err))
		}
	}()

	go func() {
		err := events.Subscribe(subCtx, func(userID string, env protocol.Envelope) {
			// Deliver to dashboards connected to THIS instance. Instances with no
			// matching connection simply send to nobody, which is why the count is
			// not treated as an error.
			hub.SendToUser(userID, env)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("event subscription ended", blog.Err(err))
		}
	}()

	return cancel
}

// startMaintenance runs the periodic sweeps and returns a stop function.
func startMaintenance(
	locks port.DistributedLock,
	sessions *app.SessionService,
	prompts *app.PromptService,
	logs port.SessionLogStore,
	tokens port.RefreshTokenStore,
	logger *slog.Logger,
) func() {
	jobCtx, cancel := context.WithCancel(context.Background())

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				// One instance per sweep. Every job below is idempotent anyway, so
				// a lock failure costs duplicated work rather than corruption.
				release, ok, err := locks.Acquire(jobCtx, "maintenance", 55*time.Second)
				if err != nil || !ok {
					continue
				}

				// A session whose device stopped reporting must be closed, or the
				// unique partial index blocks the user from ever starting another
				// on that device.
				if _, err := sessions.SweepStale(jobCtx, 3*time.Minute); err != nil {
					logger.Warn("stale session sweep failed", blog.Err(err))
				}
				// The safety net behind "Redis is disposable": redeliver anything
				// whose dispatch signal was lost.
				if _, err := prompts.ReconcilePending(jobCtx, 30*time.Second); err != nil {
					logger.Warn("prompt reconciliation failed", blog.Err(err))
				}

				release(jobCtx)
			}
		}
	}()

	// Retention runs far less often: it deletes rows, and doing so hourly is ample
	// while keeping the transaction small.
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				release, ok, err := locks.Acquire(jobCtx, "retention", 10*time.Minute)
				if err != nil || !ok {
					continue
				}
				if n, err := tokens.DeleteExpired(jobCtx, time.Now().UTC()); err != nil {
					logger.Warn("refresh token cleanup failed", blog.Err(err))
				} else if n > 0 {
					logger.Info("pruned expired refresh tokens", slog.Int64("count", n))
				}
				// Log retention is per-user configurable; a global floor is applied
				// here and the per-user policy is enforced in phase 7.
				if n, err := logs.DeleteOlderThan(jobCtx, time.Now().UTC().AddDate(0, 0, -90)); err != nil {
					logger.Warn("log retention failed", blog.Err(err))
				} else if n > 0 {
					logger.Info("pruned old session logs", slog.Int64("count", n))
				}
				release(jobCtx)
			}
		}
	}()

	return cancel
}

// runMigrations applies pending migrations and exits.
//
// A separate mode rather than something the server does at boot: during a rolling
// deploy several instances start at once, and concurrent DDL can deadlock with the
// schema half-applied. This runs as one explicit step before new containers are
// promoted.
func runMigrations(cfg *config.Config, logger *slog.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := postgres.New(cfg.Database, logger)
	if err := db.Start(ctx); err != nil {
		logger.Error("cannot connect for migration", blog.Err(err))
		return exitRuntime
	}
	defer func() { _ = db.Stop(context.Background()) }()

	if err := postgres.NewMigrator(db.Pool(), logger).Up(ctx); err != nil {
		logger.Error("migration failed", blog.Err(err))
		return exitRuntime
	}
	return exitOK
}

// warnOnDevelopmentShortcuts surfaces settings tolerable locally but defects
// elsewhere.
//
// Validate() rejects these outright outside development. Warning about them in
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
		logger.Warn("redis.url is empty; prompt dispatch falls back to reconnect delivery (development only)")
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
