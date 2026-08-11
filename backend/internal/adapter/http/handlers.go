package http

import (
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/bhuvan0808/beuviancode/backend/internal/app"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/version"
)

// refreshCookieName is the refresh token cookie.
//
// The __Host- prefix is a browser-enforced guarantee: the cookie must be Secure,
// have no Domain attribute, and have Path=/. That makes it impossible for a
// subdomain (including one an attacker controls via subdomain takeover) to set or
// overwrite it, which a plain cookie name cannot prevent.
const refreshCookieName = "__Host-beuvian_refresh"

// setRefreshCookie writes the refresh token cookie.
//
// HttpOnly so JavaScript cannot read it, which is what limits the damage of an XSS
// bug in the dashboard to actions rather than credential theft. SameSite=Lax
// because the OAuth callback is a top-level cross-site navigation and Strict would
// drop the cookie on exactly that request.
func (h *Handlers) setRefreshCookie(c *fiber.Ctx, token string, maxAge int) {
	name, path := refreshCookieName, "/"
	// The __Host- prefix requires Secure and no Domain. In development over plain
	// http the browser would reject it entirely, so fall back to an ordinary name
	// there rather than silently having no session at all.
	if !h.cfg.Auth.CookieSecure || h.cfg.Auth.CookieDomain != "" {
		name = "beuvian_refresh"
	}

	c.Cookie(&fiber.Cookie{
		Name:     name,
		Value:    token,
		Path:     path,
		Domain:   h.cfg.Auth.CookieDomain,
		MaxAge:   maxAge,
		Secure:   h.cfg.Auth.CookieSecure,
		HTTPOnly: true,
		SameSite: "Lax",
	})
}

func (h *Handlers) readRefreshCookie(c *fiber.Ctx) string {
	if v := c.Cookies(refreshCookieName); v != "" {
		return v
	}
	return c.Cookies("beuvian_refresh")
}

// Handlers holds the HTTP handlers and their dependencies.
type Handlers struct {
	auth     *app.AuthService
	devices  *app.DeviceService
	sessions *app.SessionService
	prompts  *app.PromptService
	repos    *app.RepositoryService
	notifs   *app.NotificationService
	settings *app.SettingsService
	clock    port.Clock
	conns    port.ConnectionRegistry
	health   []HealthCheck
	cfg      Config
	log      *slog.Logger
}

// HealthCheck is a named dependency probe for /health/ready.
type HealthCheck struct {
	Name  string
	Check func(c *fiber.Ctx) error
	// Critical marks a dependency whose failure means the instance cannot serve.
	// Redis is deliberately non-critical when redis.required is false: the backend
	// genuinely still works, and reporting unhealthy would pull a serving instance
	// out of rotation over a recoverable condition.
	Critical bool
}

// ---------------------------------------------------------------------------
// Authentication

// BeginGitHubLogin redirects to GitHub's authorization page.
func (h *Handlers) BeginGitHubLogin(c *fiber.Ctx) error {
	// The post-login redirect is validated against the configured dashboard
	// origin. Accepting an arbitrary value would turn the login flow into an open
	// redirect that lends our domain's credibility to a phishing page.
	redirect := h.safeRedirect(c.Query("redirect"))

	authURL, err := h.auth.BeginLogin(c.UserContext(), redirect)
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.Redirect(authURL, fiber.StatusTemporaryRedirect)
}

// safeRedirect validates a post-login redirect target against the dashboard origin.
func (h *Handlers) safeRedirect(candidate string) string {
	fallback := h.cfg.Auth.DashboardURL
	if candidate == "" {
		return fallback
	}

	target, err := url.Parse(candidate)
	if err != nil {
		return fallback
	}
	// A relative path is safe: it cannot leave our origin.
	if target.Scheme == "" && target.Host == "" && strings.HasPrefix(candidate, "/") {
		return strings.TrimRight(fallback, "/") + candidate
	}

	allowed, err := url.Parse(fallback)
	if err != nil {
		return fallback
	}
	if target.Scheme == allowed.Scheme && target.Host == allowed.Host {
		return candidate
	}
	return fallback
}

// CompleteGitHubLogin handles the OAuth callback.
func (h *Handlers) CompleteGitHubLogin(c *fiber.Ctx) error {
	// GitHub reports user denial via an `error` parameter rather than an error
	// status, so it has to be checked explicitly or it looks like a missing code.
	if e := c.Query("error"); e != "" {
		h.log.Info("github login denied by user", slog.String("error", e))
		return c.Redirect(h.cfg.Auth.DashboardURL+"?error=access_denied", fiber.StatusTemporaryRedirect)
	}

	result, err := h.auth.CompleteLogin(c.UserContext(),
		c.Query("code"), c.Query("state"), c.Get(fiber.HeaderUserAgent), c.IP())
	if err != nil {
		// Redirect rather than returning JSON: the user's browser is here after a
		// top-level navigation, and a raw JSON error page is a dead end for them.
		h.log.Warn("github login failed", slog.String("error", err.Error()))
		return c.Redirect(h.cfg.Auth.DashboardURL+"?error=login_failed", fiber.StatusTemporaryRedirect)
	}

	h.setRefreshCookie(c, result.RefreshToken, int(h.cfg.Auth.RefreshTokenTTL.Seconds()))

	// The access token goes in the fragment, not the query string. Fragments are
	// never sent to servers and stay out of access logs, Referer headers, and
	// browser history syncing.
	redirect := result.Redirect
	if redirect == "" {
		redirect = h.cfg.Auth.DashboardURL
	}
	return c.Redirect(redirect+"#access_token="+url.QueryEscape(result.AccessToken),
		fiber.StatusTemporaryRedirect)
}

// Refresh rotates the refresh token and issues a new access token.
func (h *Handlers) Refresh(c *fiber.Ctx) error {
	token := h.readRefreshCookie(c)
	if token == "" {
		return writeError(c, h.log, domain.ErrUnauthorized)
	}

	result, err := h.auth.Refresh(c.UserContext(), token, c.Get(fiber.HeaderUserAgent), c.IP())
	if err != nil {
		// Clear the cookie on failure so a browser holding a revoked token stops
		// retrying with it on every request.
		h.setRefreshCookie(c, "", -1)
		return writeError(c, h.log, err)
	}

	h.setRefreshCookie(c, result.RefreshToken, int(h.cfg.Auth.RefreshTokenTTL.Seconds()))
	return c.JSON(accessTokenResponse{
		AccessToken: result.AccessToken,
		ExpiresIn:   int(h.cfg.Auth.AccessTokenTTL.Seconds()),
		TokenType:   "Bearer",
	})
}

// Logout revokes the session.
func (h *Handlers) Logout(c *fiber.Ctx) error {
	if token := h.readRefreshCookie(c); token != "" {
		if err := h.auth.Logout(c.UserContext(), token); err != nil {
			return writeError(c, h.log, err)
		}
	}
	h.setRefreshCookie(c, "", -1)
	return c.SendStatus(fiber.StatusNoContent)
}

// Me returns the authenticated user.
func (h *Handlers) Me(c *fiber.Ctx) error {
	user, err := h.auth.Me(c.UserContext(), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toUser(user))
}

// ---------------------------------------------------------------------------
// Devices

// RegisterDevice creates a device and returns its token.
func (h *Handlers) RegisterDevice(c *fiber.Ctx) error {
	var req registerDeviceRequest
	if err := c.BodyParser(&req); err != nil {
		return writeError(c, h.log, badRequest("malformed request body"))
	}

	result, err := h.devices.Register(c.UserContext(), app.RegisterInput{
		UserID:       userID(c),
		Name:         strings.TrimSpace(req.Name),
		Platform:     strings.TrimSpace(req.Platform),
		AgentVersion: req.AgentVersion,
		Capabilities: req.Capabilities,
		IPAddress:    c.IP(),
		UserAgent:    c.Get(fiber.HeaderUserAgent),
	})
	if err != nil {
		return writeError(c, h.log, err)
	}

	return c.Status(fiber.StatusCreated).JSON(registerDeviceResponse{
		Device:      toDevice(app.DeviceView{Device: result.Device}),
		DeviceToken: result.Token,
		ExpiresAt:   result.ExpiresAt,
	})
}

// ListDevices returns the user's devices.
func (h *Handlers) ListDevices(c *fiber.Ctx) error {
	views, err := h.devices.List(c.UserContext(), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	out := make([]deviceResponse, 0, len(views))
	for _, v := range views {
		out = append(out, toDevice(v))
	}
	return c.JSON(listResponse[deviceResponse]{Data: out})
}

// GetDevice returns one device.
func (h *Handlers) GetDevice(c *fiber.Ctx) error {
	view, err := h.devices.Get(c.UserContext(), c.Params("id"), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toDevice(view))
}

// UpdateDevice renames a device.
func (h *Handlers) UpdateDevice(c *fiber.Ctx) error {
	var req updateDeviceRequest
	if err := c.BodyParser(&req); err != nil {
		return writeError(c, h.log, badRequest("malformed request body"))
	}
	device, err := h.devices.Rename(c.UserContext(), c.Params("id"), userID(c), strings.TrimSpace(req.Name))
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toDevice(app.DeviceView{Device: device}))
}

// RevokeDevice invalidates a device's credentials.
func (h *Handlers) RevokeDevice(c *fiber.Ctx) error {
	if err := h.devices.Revoke(c.UserContext(), c.Params("id"), userID(c), c.IP()); err != nil {
		return writeError(c, h.log, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// DeleteDevice removes a device.
func (h *Handlers) DeleteDevice(c *fiber.Ctx) error {
	if err := h.devices.Delete(c.UserContext(), c.Params("id"), userID(c), c.IP()); err != nil {
		return writeError(c, h.log, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Repositories

// ListRepositories returns registered repositories.
func (h *Handlers) ListRepositories(c *fiber.Ctx) error {
	repos, err := h.repos.List(c.UserContext(), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	out := make([]repositoryResponse, 0, len(repos))
	for _, r := range repos {
		out = append(out, toRepository(r))
	}
	return c.JSON(listResponse[repositoryResponse]{Data: out})
}

// AddRepository registers a repository.
func (h *Handlers) AddRepository(c *fiber.Ctx) error {
	var req addRepositoryRequest
	if err := c.BodyParser(&req); err != nil {
		return writeError(c, h.log, badRequest("malformed request body"))
	}
	repo, err := h.repos.Add(c.UserContext(), app.AddInput{
		UserID:        userID(c),
		FullName:      strings.TrimSpace(req.FullName),
		LocalPath:     req.LocalPath,
		DeviceID:      req.DeviceID,
		DefaultBranch: req.DefaultBranch,
		GitHubID:      req.GitHubID,
		IsPrivate:     req.IsPrivate,
	})
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.Status(fiber.StatusCreated).JSON(toRepository(repo))
}

// GetRepository returns one repository.
func (h *Handlers) GetRepository(c *fiber.Ctx) error {
	repo, err := h.repos.Get(c.UserContext(), c.Params("id"), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toRepository(repo))
}

// UpdateRepository changes a repository's local location.
func (h *Handlers) UpdateRepository(c *fiber.Ctx) error {
	var req updateRepositoryRequest
	if err := c.BodyParser(&req); err != nil {
		return writeError(c, h.log, badRequest("malformed request body"))
	}
	repo, err := h.repos.Update(c.UserContext(), c.Params("id"), userID(c), app.UpdateInput{
		LocalPath:     req.LocalPath,
		DeviceID:      req.DeviceID,
		DefaultBranch: req.DefaultBranch,
	})
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toRepository(repo))
}

// DeleteRepository removes a repository.
func (h *Handlers) DeleteRepository(c *fiber.Ctx) error {
	if err := h.repos.Delete(c.UserContext(), c.Params("id"), userID(c)); err != nil {
		return writeError(c, h.log, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ListGitHubRepositories returns upstream repository metadata.
func (h *Handlers) ListGitHubRepositories(c *fiber.Ctx) error {
	repos, err := h.repos.ListGitHub(c.UserContext(), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	out := make([]githubRepoResponse, 0, len(repos))
	for _, r := range repos {
		out = append(out, toGitHubRepo(r))
	}
	return c.JSON(listResponse[githubRepoResponse]{Data: out})
}

// ---------------------------------------------------------------------------
// Sessions

// StartSession begins a coding session.
func (h *Handlers) StartSession(c *fiber.Ctx) error {
	var req startSessionRequest
	if err := c.BodyParser(&req); err != nil {
		return writeError(c, h.log, badRequest("malformed request body"))
	}
	if req.Adapter == "" {
		req.Adapter = "claude" // the only implemented adapter
	}

	session, err := h.sessions.Start(c.UserContext(), app.StartInput{
		UserID:           userID(c),
		DeviceID:         req.DeviceID,
		RepositoryID:     req.RepositoryID,
		Adapter:          req.Adapter,
		WorkingDirectory: req.WorkingDirectory,
		InitialPrompt:    req.InitialPrompt,
		IPAddress:        c.IP(),
	})
	if err != nil {
		return writeError(c, h.log, err)
	}

	// An initial prompt is queued through the same durable path as any other, so
	// it survives the device being offline at start time.
	if req.InitialPrompt != "" {
		if _, err := h.prompts.Send(c.UserContext(), app.SendInput{
			UserID:    userID(c),
			DeviceID:  req.DeviceID,
			SessionID: session.ID,
			Text:      req.InitialPrompt,
			IPAddress: c.IP(),
		}); err != nil {
			// The session exists; report the failure without unwinding it.
			h.log.Warn("failed to queue initial prompt", slog.String("error", err.Error()))
		}
	}

	return c.Status(fiber.StatusCreated).JSON(toSession(session, h.clock.Now()))
}

// ListSessions returns session history.
func (h *Handlers) ListSessions(c *fiber.Ctx) error {
	sessions, next, err := h.sessions.List(c.UserContext(),
		port.SessionFilter{
			UserID:     userID(c),
			DeviceID:   c.Query("device_id"),
			ActiveOnly: c.Query("active") == "true",
		},
		port.Page{Limit: intQuery(c, "limit", 50), Cursor: c.Query("cursor")})
	if err != nil {
		return writeError(c, h.log, err)
	}

	now := h.clock.Now()
	out := make([]sessionResponse, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, toSession(s, now))
	}
	return c.JSON(listResponse[sessionResponse]{Data: out, NextCursor: next, HasMore: next != ""})
}

// GetSession returns one session.
func (h *Handlers) GetSession(c *fiber.Ctx) error {
	session, err := h.sessions.Get(c.UserContext(), c.Params("id"), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toSession(session, h.clock.Now()))
}

// StopSession requests a graceful stop.
func (h *Handlers) StopSession(c *fiber.Ctx) error {
	if err := h.sessions.Stop(c.UserContext(), c.Params("id"), userID(c), c.IP()); err != nil {
		return writeError(c, h.log, err)
	}
	return c.SendStatus(fiber.StatusAccepted)
}

// GetSessionLogs returns session output after a sequence number.
func (h *Handlers) GetSessionLogs(c *fiber.Ctx) error {
	afterSeq := int64(intQuery(c, "after_seq", 0))
	limit := intQuery(c, "limit", 200)

	logs, err := h.sessions.Logs(c.UserContext(), c.Params("id"), userID(c), afterSeq, limit)
	if err != nil {
		return writeError(c, h.log, err)
	}

	out := make([]logResponse, 0, len(logs))
	for _, l := range logs {
		out = append(out, toLog(l))
	}

	resp := logsResponse{Data: out, NextSeq: afterSeq}
	if n := len(out); n > 0 {
		resp.NextSeq = out[n-1].Seq
		// A full page implies more may exist. Cheaper than a COUNT, and the client
		// simply requests again and gets an empty page if it was exactly full.
		resp.HasMore = n >= limit
	}
	return c.JSON(resp)
}

// GetSessionMessages returns the conversation for a session.
func (h *Handlers) GetSessionMessages(c *fiber.Ctx) error {
	messages, next, err := h.sessions.Messages(c.UserContext(), c.Params("id"), userID(c),
		port.Page{Limit: intQuery(c, "limit", 100), Cursor: c.Query("cursor")})
	if err != nil {
		return writeError(c, h.log, err)
	}
	out := make([]messageResponse, 0, len(messages))
	for _, m := range messages {
		out = append(out, toMessage(m))
	}
	return c.JSON(listResponse[messageResponse]{Data: out, NextCursor: next, HasMore: next != ""})
}

// ---------------------------------------------------------------------------
// Prompts

// SendPrompt queues an instruction for a device.
//
// Returns 202, not 200. The distinction is real: the prompt is durably queued but
// not yet delivered, and an offline device is not an error — the whole point is
// that a user can send an instruction to a sleeping laptop.
func (h *Handlers) SendPrompt(c *fiber.Ctx) error {
	var req sendPromptRequest
	if err := c.BodyParser(&req); err != nil {
		return writeError(c, h.log, badRequest("malformed request body"))
	}

	prompt, err := h.prompts.Send(c.UserContext(), app.SendInput{
		UserID:    userID(c),
		DeviceID:  req.DeviceID,
		SessionID: req.SessionID,
		Text:      strings.TrimSpace(req.Text),
		IPAddress: c.IP(),
	})
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(toPrompt(prompt))
}

// ListPrompts returns queued prompts.
func (h *Handlers) ListPrompts(c *fiber.Ctx) error {
	prompts, next, err := h.prompts.List(c.UserContext(), userID(c), c.Query("device_id"),
		port.Page{Limit: intQuery(c, "limit", 50), Cursor: c.Query("cursor")})
	if err != nil {
		return writeError(c, h.log, err)
	}
	out := make([]promptResponse, 0, len(prompts))
	for _, p := range prompts {
		out = append(out, toPrompt(p))
	}
	return c.JSON(listResponse[promptResponse]{Data: out, NextCursor: next, HasMore: next != ""})
}

// GetPrompt returns one prompt.
func (h *Handlers) GetPrompt(c *fiber.Ctx) error {
	prompt, err := h.prompts.Get(c.UserContext(), c.Params("id"), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toPrompt(prompt))
}

// CancelPrompt withdraws an undelivered prompt.
func (h *Handlers) CancelPrompt(c *fiber.Ctx) error {
	if err := h.prompts.Cancel(c.UserContext(), c.Params("id"), userID(c), c.IP()); err != nil {
		return writeError(c, h.log, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Notifications and settings

// ListNotifications returns a user's notifications.
func (h *Handlers) ListNotifications(c *fiber.Ctx) error {
	notifications, next, err := h.notifs.List(c.UserContext(), userID(c),
		c.Query("unread") == "true",
		port.Page{Limit: intQuery(c, "limit", 50), Cursor: c.Query("cursor")})
	if err != nil {
		return writeError(c, h.log, err)
	}
	out := make([]notificationResponse, 0, len(notifications))
	for _, n := range notifications {
		out = append(out, toNotification(n))
	}
	return c.JSON(listResponse[notificationResponse]{Data: out, NextCursor: next, HasMore: next != ""})
}

// MarkNotificationRead marks one notification read.
func (h *Handlers) MarkNotificationRead(c *fiber.Ctx) error {
	if err := h.notifs.MarkRead(c.UserContext(), c.Params("id"), userID(c)); err != nil {
		return writeError(c, h.log, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// MarkAllNotificationsRead clears unread notifications.
func (h *Handlers) MarkAllNotificationsRead(c *fiber.Ctx) error {
	if err := h.notifs.MarkAllRead(c.UserContext(), userID(c)); err != nil {
		return writeError(c, h.log, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// GetSettings returns a user's preferences.
func (h *Handlers) GetSettings(c *fiber.Ctx) error {
	settings, err := h.settings.Get(c.UserContext(), userID(c))
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toSettings(settings))
}

// UpdateSettings patches preferences.
func (h *Handlers) UpdateSettings(c *fiber.Ctx) error {
	var req updateSettingsRequest
	if err := c.BodyParser(&req); err != nil {
		return writeError(c, h.log, badRequest("malformed request body"))
	}
	settings, err := h.settings.Update(c.UserContext(), userID(c), app.SettingsPatch{
		NotifyOnComplete:      req.NotifyOnComplete,
		NotifyOnWaiting:       req.NotifyOnWaiting,
		NotifyOnDeviceOffline: req.NotifyOnDeviceOffline,
		Theme:                 req.Theme,
		Timezone:              req.Timezone,
		LogRetentionDays:      req.LogRetentionDays,
	})
	if err != nil {
		return writeError(c, h.log, err)
	}
	return c.JSON(toSettings(settings))
}

// ---------------------------------------------------------------------------
// Health

// Health is the liveness probe.
//
// Checks nothing. It answers "is this process running?", and that is deliberate: if
// liveness checked the database, a brief Supabase blip would make the orchestrator
// kill otherwise-healthy instances and escalate a partial degradation into a full
// outage.
func (h *Handlers) Health(c *fiber.Ctx) error {
	build := version.Get()
	return c.JSON(healthResponse{
		Status:  "ok",
		Version: build.Version,
		Commit:  build.Commit,
	})
}

// Ready is the readiness probe, gating traffic on dependency health.
func (h *Handlers) Ready(c *fiber.Ctx) error {
	checks := make(map[string]string, len(h.health))
	status := "ok"
	code := fiber.StatusOK

	for _, hc := range h.health {
		if err := hc.Check(c); err != nil {
			checks[hc.Name] = "error: " + err.Error()
			if hc.Critical {
				status, code = "unavailable", fiber.StatusServiceUnavailable
			} else if status == "ok" {
				// Non-critical failure: report degraded but keep serving traffic.
				status = "degraded"
			}
			continue
		}
		checks[hc.Name] = "ok"
	}

	return c.Status(code).JSON(readyResponse{
		Status:      status,
		Checks:      checks,
		Version:     version.Get().Version,
		Connections: h.conns.Count(),
	})
}

// intQuery reads a bounded integer query parameter.
func intQuery(c *fiber.Ctx, name string, fallback int) int {
	raw := c.Query(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}
