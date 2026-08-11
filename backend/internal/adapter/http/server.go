package http

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/bhuvan0808/beuviancode/backend/internal/app"
	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/version"
)

// Config is the subset of application configuration the HTTP layer needs.
//
// A narrowed view rather than the whole *config.Config: this layer has no business
// reading the database URL, and passing only what it needs makes that structural
// rather than a matter of discipline.
type Config struct {
	Server    config.Server
	Auth      config.Auth
	CORS      config.CORS
	RateLimit config.RateLimit
	Env       config.Environment
}

// Deps groups everything the server needs.
type Deps struct {
	Config   Config
	Auth     *app.AuthService
	Devices  *app.DeviceService
	Sessions *app.SessionService
	Prompts  *app.PromptService
	Repos    *app.RepositoryService
	Notifs   *app.NotificationService
	Settings *app.SettingsService
	Limiter  port.RateLimiter
	Conns    port.ConnectionRegistry
	Clock    port.Clock
	Health   []HealthCheck

	// WebSocketHandler is registered at /v1/ws. Passed in rather than imported so
	// this package does not depend on the ws package, which would be a cycle: ws
	// needs the auth service, and both are wired together only in main.
	WebSocketHandler fiber.Handler

	Log *slog.Logger
}

// Server runs the Fiber HTTP listener as a lifecycle component.
type Server struct {
	appFiber *fiber.App
	cfg      Config
	log      *slog.Logger
}

// New builds the HTTP server and registers every route.
func New(d Deps) *Server {
	log := d.Log.With(slog.String("component", "http"))

	appFiber := fiber.New(fiber.Config{
		AppName:      "beuvian-backend " + version.Get().Version,
		ReadTimeout:  d.Config.Server.ReadTimeout,
		WriteTimeout: d.Config.Server.WriteTimeout,
		IdleTimeout:  d.Config.Server.IdleTimeout,
		BodyLimit:    d.Config.Server.BodyLimit,

		// Do not advertise the framework. It tells an attacker which CVEs to try
		// and offers nothing to legitimate clients.
		DisableStartupMessage: true,
		ServerHeader:          "",

		// Trust X-Forwarded-* only from configured proxies. Blindly trusting them
		// lets any client spoof its IP and defeat per-IP rate limiting.
		EnableTrustedProxyCheck: len(d.Config.Server.TrustedProxies) > 0,
		TrustedProxies:          d.Config.Server.TrustedProxies,
		ProxyHeader:             proxyHeader(d.Config.Server.TrustedProxies),

		// Route every error through our single error shape, including the ones
		// Fiber raises itself (404, 405, body-limit exceeded).
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return writeError(c, log, err)
		},
	})

	h := &Handlers{
		auth: d.Auth, devices: d.Devices, sessions: d.Sessions, prompts: d.Prompts,
		repos: d.Repos, notifs: d.Notifs, settings: d.Settings,
		clock: d.Clock, conns: d.Conns, health: d.Health,
		cfg: d.Config, log: log,
	}

	registerRoutes(appFiber, h, d, log)

	return &Server{appFiber: appFiber, cfg: d.Config, log: log}
}

// proxyHeader returns the header to read the client IP from.
//
// Empty when no proxies are trusted, so Fiber falls back to the socket's remote
// address — the only value a client cannot forge.
func proxyHeader(trusted []string) string {
	if len(trusted) == 0 {
		return ""
	}
	return fiber.HeaderXForwardedFor
}

// registerRoutes wires the API surface.
//
// Grouped by authentication requirement rather than by resource, so it is visible
// at a glance which routes are public. A route accidentally added outside the
// protected group is obvious here in a way it would not be if middleware were
// applied per-route.
func registerRoutes(f *fiber.App, h *Handlers, d Deps, log *slog.Logger) {
	// Global middleware, in deliberate order: recover outermost so it catches
	// panics from everything after it; request context before logging so the log
	// line carries the IDs.
	f.Use(recoverPanic(log))
	f.Use(requestContext())
	f.Use(securityHeaders(d.Config.Env.IsProduction()))
	f.Use(corsMiddleware(d.Config.CORS))
	f.Use(requestLogger(log))

	// Health endpoints sit outside /v1 and outside rate limiting: an orchestrator
	// probing every few seconds must never be throttled, or a busy instance gets
	// killed for being busy.
	f.Get("/health", h.Health)
	f.Get("/health/ready", h.Ready)

	v1 := f.Group("/v1")

	// WebSocket. Authenticated by the protocol's own AUTH frame rather than by
	// HTTP middleware, because a browser cannot set an Authorization header on a
	// WebSocket handshake.
	if d.WebSocketHandler != nil {
		v1.Get("/ws", d.WebSocketHandler)
	}

	// --- Public: authentication ---
	//
	// A tighter rate limit than the rest of the API. These are the endpoints worth
	// brute-forcing, so they get the smaller budget.
	authLimit := rateLimit(d.Limiter, log,
		d.Config.RateLimit.LoginRequests, d.Config.RateLimit.LoginWindow, "auth")

	auth := v1.Group("/auth")
	auth.Get("/github", authLimit, h.BeginGitHubLogin)
	auth.Get("/github/callback", authLimit, h.CompleteGitHubLogin)
	auth.Post("/refresh", authLimit, h.Refresh)
	auth.Post("/logout", h.Logout)

	// --- Protected ---
	protected := v1.Group("", requireAuth(d.Auth, log))
	if d.Config.RateLimit.Enabled {
		protected.Use(rateLimit(d.Limiter, log,
			d.Config.RateLimit.Requests, d.Config.RateLimit.Window, "api"))
	}

	protected.Get("/auth/me", h.Me)

	devices := protected.Group("/devices")
	devices.Post("/register", h.RegisterDevice)
	devices.Get("/", h.ListDevices)
	devices.Get("/:id", h.GetDevice)
	devices.Patch("/:id", h.UpdateDevice)
	devices.Post("/:id/revoke", h.RevokeDevice)
	devices.Delete("/:id", h.DeleteDevice)

	repos := protected.Group("/repositories")
	// Registered before /:id so "github" is not captured as an ID.
	repos.Get("/github", h.ListGitHubRepositories)
	repos.Get("/", h.ListRepositories)
	repos.Post("/", h.AddRepository)
	repos.Get("/:id", h.GetRepository)
	repos.Patch("/:id", h.UpdateRepository)
	repos.Delete("/:id", h.DeleteRepository)

	sessions := protected.Group("/sessions")
	sessions.Post("/", h.StartSession)
	sessions.Get("/", h.ListSessions)
	sessions.Get("/:id", h.GetSession)
	sessions.Post("/:id/stop", h.StopSession)
	sessions.Get("/:id/logs", h.GetSessionLogs)
	sessions.Get("/:id/messages", h.GetSessionMessages)

	prompts := protected.Group("/prompts")
	prompts.Post("/", h.SendPrompt)
	prompts.Get("/", h.ListPrompts)
	prompts.Get("/:id", h.GetPrompt)
	prompts.Delete("/:id", h.CancelPrompt)

	notifications := protected.Group("/notifications")
	notifications.Get("/", h.ListNotifications)
	notifications.Post("/read-all", h.MarkAllNotificationsRead)
	notifications.Post("/:id/read", h.MarkNotificationRead)

	settings := protected.Group("/settings")
	settings.Get("/", h.GetSettings)
	settings.Patch("/", h.UpdateSettings)
}

// Name identifies the component in lifecycle logs.
func (s *Server) Name() string { return "httpserver" }

// Start begins listening.
//
// Non-blocking, as lifecycle.Component requires: the listener runs in a goroutine
// so the supervisor can continue starting components. A blocking Start here would
// deadlock the whole boot sequence.
func (s *Server) Start(context.Context) error {
	addr := s.cfg.Server.Addr()

	// Bind synchronously before returning, so a port conflict is reported as a
	// startup failure rather than appearing later as a silently dead listener.
	ln, err := listen(addr)
	if err != nil {
		return err
	}

	go func() {
		if err := s.appFiber.Listener(ln); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("http listener stopped", slog.String("error", err.Error()))
		}
	}()

	s.log.Info("listening", slog.String("addr", addr))
	return nil
}

// Stop drains in-flight requests within the grace period.
//
// The supervisor stops this component before the database and Redis, so requests
// finish against live dependencies instead of failing on every deploy.
func (s *Server) Stop(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	timeout := 10 * time.Second
	if ok {
		timeout = time.Until(deadline)
	}
	return s.appFiber.ShutdownWithTimeout(timeout)
}
