package http

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/bhuvan0808/beuviancode/backend/internal/app"
	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
)

// Fiber locals keys. Typed constants rather than bare strings so a typo in one
// handler is not a silent nil lookup.
const (
	localUserID    = "beuvian_user_id"
	localRequestID = "beuvian_request_id"
)

// requestContext attaches request and correlation IDs.
//
// The request ID identifies this one HTTP call. The correlation ID spans the whole
// causal chain across processes: dashboard click, API call, Redis enqueue,
// WebSocket delivery, agent injection, resulting log lines. One grep then
// reconstructs the entire story, which is what makes "I sent a prompt and nothing
// happened" answerable.
//
// A client-supplied X-Request-ID is honoured so a dashboard can correlate its own
// telemetry with ours, but it is length-capped: an unbounded header value would end
// up in every log line for that request.
func requestContext() fiber.Handler {
	return func(c *fiber.Ctx) error {
		requestID := c.Get("X-Request-ID")
		if requestID == "" || len(requestID) > 128 {
			requestID = id.WithPrefix(id.PrefixRequest)
		}
		correlationID := c.Get("X-Correlation-ID")
		if correlationID == "" || len(correlationID) > 128 {
			correlationID = id.WithPrefix(id.PrefixCorrelation)
		}

		ctx := blog.WithRequestID(c.UserContext(), requestID)
		ctx = blog.WithCorrelationID(ctx, correlationID)
		c.SetUserContext(ctx)

		c.Locals(localRequestID, requestID)
		// Echoed so a client can quote it in a bug report.
		c.Set("X-Request-ID", requestID)
		c.Set("X-Correlation-ID", correlationID)

		return c.Next()
	}
}

// requestLogger emits one structured line per request.
//
// One line per request, not one per phase: request logs are the highest-volume
// thing the backend writes, and multiplying them makes them expensive to store and
// harder to read.
func requestLogger(log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		status := c.Response().StatusCode()

		attrs := []any{
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.Duration("took", time.Since(start)),
			slog.String("ip", c.IP()),
		}
		if rid, ok := c.Locals(localRequestID).(string); ok {
			attrs = append(attrs, slog.String(blog.FieldRequestID, rid))
		}
		if uid, ok := c.Locals(localUserID).(string); ok && uid != "" {
			attrs = append(attrs, slog.String(blog.FieldUserID, uid))
		}

		// Health checks are logged at debug: they fire every few seconds and would
		// otherwise drown out real traffic.
		switch {
		case strings.HasPrefix(c.Path(), "/health"):
			log.Debug("request", attrs...)
		case status >= 500:
			log.Error("request", attrs...)
		case status >= 400:
			log.Warn("request", attrs...)
		default:
			log.Info("request", attrs...)
		}
		return err
	}
}

// recoverPanic converts a panic into a 500 instead of killing the process.
//
// Fiber ships a recover middleware, but this one logs the panic with our
// correlation fields attached, which is the difference between a diagnosable
// incident and a stack trace with no context.
func recoverPanic(log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered",
					slog.Any("panic", r),
					slog.String("path", c.Path()),
					slog.String("method", c.Method()))
				err = fiber.NewError(fiber.StatusInternalServerError, "internal error")
			}
		}()
		return c.Next()
	}
}

// requireAuth rejects requests without a valid dashboard access token.
//
// Verifies the token is an ACCESS token specifically. Device tokens are signed by
// the same secret, so without that check a leaked agent credential would grant full
// dashboard access, which is exactly what the two token kinds exist to prevent.
func requireAuth(auth *app.AuthService, log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		header := c.Get(fiber.HeaderAuthorization)
		if header == "" {
			return writeError(c, log, domain.ErrUnauthorized)
		}
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			return writeError(c, log, domain.ErrUnauthorized)
		}

		claims, err := auth.VerifyAccess(token)
		if err != nil {
			return writeError(c, log, err)
		}

		c.Locals(localUserID, claims.Subject)
		c.SetUserContext(blog.WithCorrelationID(
			c.UserContext(), blog.CorrelationIDFrom(c.UserContext())))
		return c.Next()
	}
}

// userID returns the authenticated user, or "" when unauthenticated.
func userID(c *fiber.Ctx) string {
	s, _ := c.Locals(localUserID).(string)
	return s
}

// rateLimit enforces a per-identity quota.
//
// Keyed on the user ID when authenticated and the client IP otherwise, so one
// abusive account cannot consume the budget of everyone behind a shared NAT, and an
// unauthenticated flood is still bounded.
//
// The limiter fails OPEN when Redis is unavailable. That is an availability choice
// made deliberately: failing closed would turn a cache outage into a total outage.
// The production config check refuses to start without Redis precisely because this
// trade means an unnoticed Redis failure silently disables rate limiting.
func rateLimit(limiter port.RateLimiter, log *slog.Logger, limit int, window time.Duration, scope string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		identity := userID(c)
		if identity == "" {
			identity = "ip:" + c.IP()
		}
		key := scope + ":" + identity

		allowed, remaining, resetAt, err := limiter.Allow(c.UserContext(), key, limit, window)
		if err != nil {
			log.Warn("rate limiter error; allowing request", blog.Err(err))
			return c.Next()
		}

		c.Set("X-RateLimit-Limit", strconv.Itoa(limit))
		c.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		c.Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

		if !allowed {
			// Retry-After in seconds, so a well-behaved client backs off for
			// exactly as long as the window has left rather than guessing.
			c.Set("Retry-After", strconv.Itoa(int(time.Until(resetAt).Seconds())+1))
			return writeError(c, log, domain.ErrRateLimited)
		}
		return c.Next()
	}
}

// corsMiddleware applies the configured cross-origin policy.
//
// Hand-rolled rather than using Fiber's built-in, for one reason that matters: it
// reflects only origins on the configured allowlist and never echoes an arbitrary
// Origin header. Echoing the request's origin alongside
// Access-Control-Allow-Credentials effectively disables the same-origin policy for
// any site that asks, and it is an easy configuration to arrive at by accident.
func corsMiddleware(cfg config.CORS) fiber.Handler {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	wildcard := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			wildcard = true
			continue
		}
		allowed[strings.TrimRight(strings.TrimSpace(o), "/")] = true
	}

	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	maxAge := strconv.Itoa(int(cfg.MaxAge.Seconds()))

	return func(c *fiber.Ctx) error {
		origin := strings.TrimRight(c.Get("Origin"), "/")

		if origin != "" && (allowed[origin] || wildcard) {
			// Echo the specific origin, never "*", because "*" is incompatible
			// with credentialed requests and our refresh cookie is credentialed.
			c.Set("Access-Control-Allow-Origin", origin)
			if cfg.AllowCredentials {
				c.Set("Access-Control-Allow-Credentials", "true")
			}
			// Tells caches that the response varies by origin. Without it a shared
			// cache can serve one origin's CORS headers to another.
			c.Set("Vary", "Origin")
		}

		if c.Method() == fiber.MethodOptions {
			c.Set("Access-Control-Allow-Methods", methods)
			c.Set("Access-Control-Allow-Headers", headers)
			c.Set("Access-Control-Max-Age", maxAge)
			return c.SendStatus(fiber.StatusNoContent)
		}
		return c.Next()
	}
}

// securityHeaders sets defensive response headers.
//
// The API serves only JSON, so a strict policy costs nothing here and closes off
// the class of attacks that rely on a browser interpreting a response as something
// other than data.
func securityHeaders(isProduction bool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Stops a browser from second-guessing Content-Type, which is how a JSON
		// response gets executed as script.
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("X-Frame-Options", "DENY")
		c.Set("Referrer-Policy", "no-referrer")
		// The API returns no HTML, so nothing legitimate needs to load.
		c.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")

		if isProduction {
			// Only in production: sending HSTS from a local http listener would
			// pin a developer's browser to https for localhost.
			c.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		return c.Next()
	}
}
