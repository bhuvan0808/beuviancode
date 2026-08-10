package protocol

import "time"

// AuthPayload is the first frame an agent sends after the socket opens.
//
// The connection is unauthenticated until the backend replies with Ack. An agent
// that sends anything other than AUTH first is disconnected — this keeps
// unauthenticated sockets from consuming gateway resources.
type AuthPayload struct {
	// Token is the device access token minted at registration. It is a JWT
	// scoped to a single device, NOT the user's dashboard session token: a
	// leaked device token must not grant dashboard access.
	Token string `json:"token"`

	// DeviceID is the stable, locally generated device identifier.
	DeviceID string `json:"device_id"`

	// Nonce is a single-use random value. The backend caches recently seen
	// nonces for MaxClockSkew and rejects duplicates, which defeats replay of a
	// captured AUTH frame within the freshness window.
	Nonce string `json:"nonce"`

	AgentVersion string `json:"agent_version"`
	Platform     string `json:"platform"` // "windows/amd64"
	Hostname     string `json:"hostname"`

	// Capabilities lists adapter names this build supports ("claude", ...). The
	// backend uses it to avoid dispatching a prompt to an agent that cannot
	// service it, and the dashboard uses it to grey out unavailable adapters.
	Capabilities []string `json:"capabilities,omitempty"`
}

// StatusPayload is a snapshot of what the local coding agent is doing.
//
// Sent on every state transition and at least once per heartbeat, so the
// dashboard converges to the truth even if a transition frame was lost.
type StatusPayload struct {
	State AgentState `json:"state"`

	// Adapter is the coding agent in use ("claude").
	Adapter string `json:"adapter"`

	Repository       string `json:"repository,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	CurrentTask      string `json:"current_task,omitempty"`

	// Resource usage of the supervised process, for the dashboard's CPU and
	// memory readouts.
	CPUPercent  float64 `json:"cpu_percent"`
	MemoryBytes uint64  `json:"memory_bytes"`

	// ElapsedSeconds is how long the current session has been running. Sent as
	// a duration rather than a start timestamp so the dashboard need not trust
	// the agent's wall clock.
	ElapsedSeconds int64 `json:"elapsed_seconds"`

	PID int `json:"pid,omitempty"`

	// QueuedPrompts is the depth of the agent's local offline queue.
	QueuedPrompts int `json:"queued_prompts"`
}

// LogStream identifies which output stream a log line came from.
type LogStream string

const (
	StreamStdout LogStream = "stdout"
	StreamStderr LogStream = "stderr"
	// StreamSystem is Beuvian's own commentary ("injected prompt", "reconnected")
	// interleaved into the session transcript so the dashboard terminal reads as
	// one coherent story.
	StreamSystem LogStream = "system"
)

// LogPayload carries one or more output lines from the supervised process.
//
// Lines are batched rather than sent one frame per line: a verbose build can emit
// thousands of lines per second, and one frame each would saturate the socket and
// the database. The agent flushes on a short timer or when the batch fills.
type LogPayload struct {
	Stream LogStream `json:"stream"`
	Lines  []string  `json:"lines"`

	// At is when the first line in the batch was produced.
	At time.Time `json:"at"`

	// Truncated is set when lines were dropped because the agent's ring buffer
	// overflowed. Surfacing this is a correctness requirement: a silently
	// truncated transcript would read as a complete one.
	Truncated bool `json:"truncated,omitempty"`
}

// PromptPayload is a prompt the user submitted from the dashboard, forwarded to
// the agent for injection into the coding agent's stdin.
type PromptPayload struct {
	// PromptID is the prompt_queue row ID. The agent echoes it in its Ack so
	// the backend can mark the row delivered exactly once.
	PromptID string `json:"prompt_id"`

	Text string `json:"text"`

	// EnqueuedAt is when the user submitted it, which may be much earlier than
	// delivery if the device was offline.
	EnqueuedAt time.Time `json:"enqueued_at"`

	// Attempt counts delivery attempts, starting at 1. Lets the agent recognise
	// a redelivery and avoid injecting the same prompt twice.
	Attempt int `json:"attempt"`
}

// TaskCompletePayload reports that the coding agent finished its work.
type TaskCompletePayload struct {
	TaskID string `json:"task_id,omitempty"`

	// ExitCode is the supervised process's exit code, or -1 if it is still
	// running and merely became idle (Claude Code stays alive between tasks).
	ExitCode int `json:"exit_code"`

	DurationSeconds int64 `json:"duration_seconds"`

	// Summary is the tail of the transcript, for the completion notification.
	Summary string `json:"summary,omitempty"`
}

// WaitReason explains why the coding agent stopped and needs a human.
type WaitReason string

const (
	// WaitPrompt is the ordinary case: the task finished and the agent is idle
	// at its input prompt.
	WaitPrompt WaitReason = "awaiting_prompt"
	// WaitConfirmation means a tool or permission prompt needs a yes/no.
	WaitConfirmation WaitReason = "awaiting_confirmation"
	// WaitError means it stopped on an error and cannot proceed unattended.
	WaitError WaitReason = "awaiting_error_resolution"
)

// TaskWaitingPayload signals that the coding agent is blocked on human input.
//
// This is the event that makes Beuvian useful: it is what triggers the "Claude
// is waiting for you" notification on the user's phone.
type TaskWaitingPayload struct {
	Reason WaitReason `json:"reason"`

	// Question is the text the coding agent is waiting on, when detectable.
	Question string `json:"question,omitempty"`

	DetectedAt time.Time `json:"detected_at"`
}

// DevicePresencePayload accompanies DEVICE_ONLINE and DEVICE_OFFLINE.
type DevicePresencePayload struct {
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name,omitempty"`
	At         time.Time `json:"at"`

	// Graceful distinguishes a clean shutdown from a heartbeat timeout, so the
	// dashboard can say "went offline" versus "lost connection".
	Graceful bool `json:"graceful,omitempty"`
}

// HeartbeatPayload accompanies PING and PONG.
type HeartbeatPayload struct {
	// Nonce is echoed unchanged from PING into PONG so a peer can match a reply
	// to its own probe and measure round-trip latency.
	Nonce  string    `json:"nonce"`
	SentAt time.Time `json:"sent_at"`
}

// ErrorPayload reports a failure to the peer.
type ErrorPayload struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`

	// Retryable tells the peer whether backing off and retrying can succeed.
	// A non-retryable error (bad token, unsupported version) must stop the
	// reconnect loop, otherwise the agent hammers the gateway forever.
	Retryable bool `json:"retryable"`
}

// AckPayload confirms receipt of a specific message.
//
// Ack is the universal positive reply: it answers AUTH, PROMPT delivery, and any
// other message whose sender needs to know it landed.
type AckPayload struct {
	// AckID is the ID of the envelope being acknowledged.
	AckID string `json:"ack_id"`

	// Accepted is false when the message was received but rejected; Reason then
	// explains why.
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// NotificationSeverity ranks a notification for display and delivery policy.
type NotificationSeverity string

const (
	SeverityInfo    NotificationSeverity = "info"
	SeverityWarning NotificationSeverity = "warning"
	SeverityError   NotificationSeverity = "error"
)

// NotificationPayload is a user-facing notification pushed to the dashboard.
//
// Kind is a stable machine-readable string ("task_complete", "device_offline")
// rather than a free-form label, because the future push/WhatsApp/Telegram
// channels listed in PROJECT.md must route on it without parsing prose.
type NotificationPayload struct {
	NotificationID string               `json:"notification_id"`
	Kind           string               `json:"kind"`
	Title          string               `json:"title"`
	Body           string               `json:"body,omitempty"`
	Severity       NotificationSeverity `json:"severity"`
	CreatedAt      time.Time            `json:"created_at"`
}
