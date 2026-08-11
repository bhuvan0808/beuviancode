package http

import (
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/app"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
)

// Wire types for the REST API.
//
// These are deliberately separate from the domain entities rather than
// serialising those directly. It looks like duplication and is not: the wire
// format is a public contract with its own compatibility requirements. Marshalling
// entities straight out means any internal rename becomes a breaking API change and
// any new internal field is published by accident. The explicit mapping below is
// the seam that keeps refactoring safe.

// listResponse is the envelope for every collection endpoint.
type listResponse[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// userResponse is the authenticated user.
//
// Note what is absent: the GitHub OAuth token, the numeric GitHub ID, and the
// organisation. None are needed by the dashboard, and an API should not publish
// what it does not owe.
type userResponse struct {
	ID          string    `json:"id"`
	GitHubLogin string    `json:"github_login"`
	Name        string    `json:"name"`
	Email       string    `json:"email,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func toUser(u domain.User) userResponse {
	return userResponse{
		ID:          u.ID,
		GitHubLogin: u.GitHubLogin,
		Name:        u.DisplayName(),
		Email:       u.Email,
		AvatarURL:   u.AvatarURL,
		CreatedAt:   u.CreatedAt,
	}
}

// accessTokenResponse is returned by login and refresh.
type accessTokenResponse struct {
	AccessToken string `json:"access_token"`
	// ExpiresIn is seconds rather than an absolute time, so a client with a skewed
	// clock still refreshes at the right moment.
	ExpiresIn int    `json:"expires_in"`
	TokenType string `json:"token_type"`
}

// statusResponse is a device's latest reported state.
type statusResponse struct {
	State         string    `json:"state"`
	Adapter       string    `json:"adapter,omitempty"`
	Repository    string    `json:"repository,omitempty"`
	CurrentTask   string    `json:"current_task,omitempty"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryBytes   uint64    `json:"memory_bytes"`
	QueuedPrompts int       `json:"queued_prompts"`
	SessionID     string    `json:"session_id,omitempty"`
	ReportedAt    time.Time `json:"reported_at"`
}

// deviceResponse is a device with its live state.
//
// TokenHash is conspicuously absent. It is a credential derivative and has no
// business leaving the server.
type deviceResponse struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Platform     string          `json:"platform"`
	AgentVersion string          `json:"agent_version,omitempty"`
	Capabilities []string        `json:"capabilities"`
	Online       bool            `json:"online"`
	Revoked      bool            `json:"revoked"`
	LastSeenAt   *time.Time      `json:"last_seen_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	Status       *statusResponse `json:"status,omitempty"`
}

func toDevice(v app.DeviceView) deviceResponse {
	caps := v.Device.Capabilities
	if caps == nil {
		caps = []string{} // emit [] rather than null so clients can iterate freely
	}
	out := deviceResponse{
		ID:           v.Device.ID,
		Name:         v.Device.Name,
		Platform:     v.Device.Platform,
		AgentVersion: v.Device.AgentVersion,
		Capabilities: caps,
		Online:       v.Online,
		Revoked:      v.Device.Revoked(),
		LastSeenAt:   v.Device.LastSeenAt,
		CreatedAt:    v.Device.CreatedAt,
	}
	if v.HasStatus {
		out.Status = &statusResponse{
			State:         v.Status.State,
			Adapter:       v.Status.Adapter,
			Repository:    v.Status.Repository,
			CurrentTask:   v.Status.CurrentTask,
			CPUPercent:    v.Status.CPUPercent,
			MemoryBytes:   v.Status.MemoryBytes,
			QueuedPrompts: v.QueuedPrompts,
			SessionID:     v.Status.SessionID,
			ReportedAt:    v.Status.ReportedAt,
		}
	}
	return out
}

// registerDeviceRequest registers a new agent installation.
type registerDeviceRequest struct {
	Name         string   `json:"name"`
	Platform     string   `json:"platform"`
	AgentVersion string   `json:"agent_version"`
	Capabilities []string `json:"capabilities"`
}

// registerDeviceResponse returns the device and its token.
type registerDeviceResponse struct {
	Device deviceResponse `json:"device"`
	// DeviceToken is returned ONCE. Only its hash is stored, so it can never be
	// retrieved again; losing it means re-registering.
	DeviceToken string    `json:"device_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// updateDeviceRequest renames a device.
type updateDeviceRequest struct {
	Name string `json:"name"`
}

// repositoryResponse is a registered repository.
type repositoryResponse struct {
	ID            string    `json:"id"`
	FullName      string    `json:"full_name"`
	LocalPath     string    `json:"local_path,omitempty"`
	DeviceID      string    `json:"device_id,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	IsPrivate     bool      `json:"is_private"`
	Located       bool      `json:"located"`
	CreatedAt     time.Time `json:"created_at"`
}

func toRepository(r domain.Repository) repositoryResponse {
	return repositoryResponse{
		ID:            r.ID,
		FullName:      r.FullName,
		LocalPath:     r.LocalPath,
		DeviceID:      r.DeviceID,
		DefaultBranch: r.DefaultBranch,
		IsPrivate:     r.IsPrivate,
		Located:       r.Located(),
		CreatedAt:     r.CreatedAt,
	}
}

// githubRepoResponse is upstream repository metadata.
type githubRepoResponse struct {
	ID            int64     `json:"id"`
	FullName      string    `json:"full_name"`
	DefaultBranch string    `json:"default_branch"`
	Private       bool      `json:"private"`
	Description   string    `json:"description,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toGitHubRepo(r port.GitHubRepo) githubRepoResponse {
	return githubRepoResponse(r)
}

// addRepositoryRequest registers a repository.
type addRepositoryRequest struct {
	FullName      string `json:"full_name"`
	LocalPath     string `json:"local_path"`
	DeviceID      string `json:"device_id"`
	DefaultBranch string `json:"default_branch"`
	GitHubID      int64  `json:"github_id"`
	IsPrivate     bool   `json:"is_private"`
}

// updateRepositoryRequest changes a repository's local location.
//
// Pointers distinguish "not supplied" from "set to empty". Without that, a PATCH
// omitting local_path would clear it: a data-loss bug that looks correct in the
// common case where the client sends every field.
type updateRepositoryRequest struct {
	LocalPath     *string `json:"local_path"`
	DeviceID      *string `json:"device_id"`
	DefaultBranch *string `json:"default_branch"`
}

// sessionResponse is a coding session.
type sessionResponse struct {
	ID               string     `json:"id"`
	DeviceID         string     `json:"device_id"`
	RepositoryID     string     `json:"repository_id,omitempty"`
	Adapter          string     `json:"adapter"`
	State            string     `json:"state"`
	CurrentTask      string     `json:"current_task,omitempty"`
	WorkingDirectory string     `json:"working_directory"`
	ExitCode         *int       `json:"exit_code"`
	Active           bool       `json:"active"`
	ElapsedSeconds   int64      `json:"elapsed_seconds"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
}

func toSession(s domain.Session, now time.Time) sessionResponse {
	return sessionResponse{
		ID:               s.ID,
		DeviceID:         s.DeviceID,
		RepositoryID:     s.RepositoryID,
		Adapter:          s.Adapter,
		State:            s.State.String(),
		CurrentTask:      s.CurrentTask,
		WorkingDirectory: s.WorkingDirectory,
		ExitCode:         s.ExitCode,
		Active:           s.Active(),
		ElapsedSeconds:   int64(s.Elapsed(now).Seconds()),
		StartedAt:        s.StartedAt,
		EndedAt:          s.EndedAt,
	}
}

// startSessionRequest begins a coding session.
type startSessionRequest struct {
	DeviceID         string `json:"device_id"`
	RepositoryID     string `json:"repository_id"`
	Adapter          string `json:"adapter"`
	WorkingDirectory string `json:"working_directory"`
	InitialPrompt    string `json:"initial_prompt"`
}

// logResponse is one batch of session output.
type logResponse struct {
	Seq       int64     `json:"seq"`
	Stream    string    `json:"stream"`
	Content   string    `json:"content"`
	Truncated bool      `json:"truncated"`
	At        time.Time `json:"at"`
}

func toLog(l domain.SessionLog) logResponse {
	return logResponse{
		Seq:       l.Seq,
		Stream:    string(l.Stream),
		Content:   l.Content,
		Truncated: l.Truncated,
		At:        l.At,
	}
}

// logsResponse pages by sequence rather than cursor.
//
// Timestamps collide under load and are not monotonic across a clock adjustment,
// so paging by them can silently skip lines. `seq` is a per-session counter with
// neither problem.
type logsResponse struct {
	Data    []logResponse `json:"data"`
	NextSeq int64         `json:"next_seq"`
	HasMore bool          `json:"has_more"`
}

// messageResponse is one conversation entry.
type messageResponse struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	PromptID  string    `json:"prompt_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func toMessage(m domain.Message) messageResponse {
	return messageResponse{
		ID:        m.ID,
		Role:      string(m.Role),
		Content:   m.Content,
		PromptID:  m.PromptID,
		CreatedAt: m.CreatedAt,
	}
}

// sendPromptRequest submits an instruction to a device.
type sendPromptRequest struct {
	DeviceID  string `json:"device_id"`
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

// promptResponse is a queued prompt.
type promptResponse struct {
	ID            string     `json:"id"`
	DeviceID      string     `json:"device_id"`
	SessionID     string     `json:"session_id,omitempty"`
	Text          string     `json:"text"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	EnqueuedAt    time.Time  `json:"enqueued_at"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	Error         string     `json:"error,omitempty"`
	CorrelationID string     `json:"correlation_id,omitempty"`
}

func toPrompt(p domain.QueuedPrompt) promptResponse {
	return promptResponse{
		ID:            p.ID,
		DeviceID:      p.DeviceID,
		SessionID:     p.SessionID,
		Text:          p.Text,
		Status:        string(p.Status),
		Attempts:      p.Attempts,
		EnqueuedAt:    p.EnqueuedAt,
		DeliveredAt:   p.DeliveredAt,
		Error:         p.Error,
		CorrelationID: p.CorrelationID,
	}
}

// notificationResponse is a user-facing notification.
type notificationResponse struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Body      string     `json:"body,omitempty"`
	Severity  string     `json:"severity"`
	DeviceID  string     `json:"device_id,omitempty"`
	SessionID string     `json:"session_id,omitempty"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

func toNotification(n domain.Notification) notificationResponse {
	return notificationResponse{
		ID:        n.ID,
		Kind:      n.Kind,
		Title:     n.Title,
		Body:      n.Body,
		Severity:  string(n.Severity),
		DeviceID:  n.DeviceID,
		SessionID: n.SessionID,
		ReadAt:    n.ReadAt,
		CreatedAt: n.CreatedAt,
	}
}

// settingsResponse is a user's preferences.
type settingsResponse struct {
	NotifyOnComplete      bool   `json:"notify_on_complete"`
	NotifyOnWaiting       bool   `json:"notify_on_waiting"`
	NotifyOnDeviceOffline bool   `json:"notify_on_device_offline"`
	Theme                 string `json:"theme"`
	Timezone              string `json:"timezone"`
	LogRetentionDays      int    `json:"log_retention_days"`
}

func toSettings(s domain.UserSettings) settingsResponse {
	return settingsResponse{
		NotifyOnComplete:      s.NotifyOnComplete,
		NotifyOnWaiting:       s.NotifyOnWaiting,
		NotifyOnDeviceOffline: s.NotifyOnDeviceOffline,
		Theme:                 s.Theme,
		Timezone:              s.Timezone,
		LogRetentionDays:      s.LogRetentionDays,
	}
}

// updateSettingsRequest patches preferences. Pointers for the same reason as
// updateRepositoryRequest.
type updateSettingsRequest struct {
	NotifyOnComplete      *bool   `json:"notify_on_complete"`
	NotifyOnWaiting       *bool   `json:"notify_on_waiting"`
	NotifyOnDeviceOffline *bool   `json:"notify_on_device_offline"`
	Theme                 *string `json:"theme"`
	Timezone              *string `json:"timezone"`
	LogRetentionDays      *int    `json:"log_retention_days"`
}

// healthResponse is the liveness payload.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

// readyResponse is the readiness payload, including dependency checks.
type readyResponse struct {
	Status      string            `json:"status"`
	Checks      map[string]string `json:"checks"`
	Version     string            `json:"version"`
	Connections int               `json:"connections"`
}
