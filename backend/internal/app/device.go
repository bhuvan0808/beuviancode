package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/adapter/auth"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// DeviceService manages device registration, presence, and revocation.
type DeviceService struct {
	devices  port.DeviceStore
	sessions port.SessionStore
	prompts  port.PromptQueueStore
	presence port.PresenceTracker
	conns    port.ConnectionRegistry
	issuer   port.TokenIssuer
	verifier port.TokenVerifier
	audit    port.AuditLogger
	ids      port.IDGenerator
	clock    port.Clock
	log      *slog.Logger
}

// DeviceDeps groups DeviceService's collaborators.
type DeviceDeps struct {
	Devices  port.DeviceStore
	Sessions port.SessionStore
	Prompts  port.PromptQueueStore
	Presence port.PresenceTracker
	Conns    port.ConnectionRegistry
	Issuer   port.TokenIssuer
	Verifier port.TokenVerifier
	Audit    port.AuditLogger
	IDs      port.IDGenerator
	Clock    port.Clock
	Log      *slog.Logger
}

// NewDeviceService builds a DeviceService.
func NewDeviceService(d DeviceDeps) *DeviceService {
	return &DeviceService{
		devices: d.Devices, sessions: d.Sessions, prompts: d.Prompts,
		presence: d.Presence, conns: d.Conns, issuer: d.Issuer, verifier: d.Verifier,
		audit: d.Audit, ids: d.IDs, clock: d.Clock,
		log: d.Log.With(slog.String("service", "device")),
	}
}

// RegisterInput describes a device registration request.
type RegisterInput struct {
	UserID       string
	Name         string
	Platform     string
	AgentVersion string
	Capabilities []string
	IPAddress    string
	UserAgent    string
}

// RegisterResult carries the registration outcome.
type RegisterResult struct {
	Device domain.Device
	// Token is the plaintext device token. Returned ONCE and never retrievable
	// again, because only its hash is stored.
	Token     string
	ExpiresAt time.Time
}

// Register creates a device and mints its token.
//
// This is the one place the two credential families touch: it requires a user
// access token and returns a device token. Everything afterwards uses the device
// token alone, so an agent never holds a credential that could act as the user.
func (s *DeviceService) Register(ctx context.Context, in RegisterInput) (RegisterResult, error) {
	now := s.clock.Now()

	device := domain.Device{
		ID:           s.ids.NewID(id.PrefixDevice),
		UserID:       in.UserID,
		Name:         in.Name,
		Platform:     in.Platform,
		AgentVersion: in.AgentVersion,
		Capabilities: in.Capabilities,
		CreatedAt:    now,
	}
	if device.Capabilities == nil {
		device.Capabilities = []string{}
	}
	if err := device.Validate(); err != nil {
		return RegisterResult{}, err
	}

	token, claims, err := s.issuer.IssueDevice(in.UserID, device.ID)
	if err != nil {
		return RegisterResult{}, err
	}
	// Only the hash is persisted. A database dump therefore yields no working
	// device credentials.
	device.TokenHash = auth.HashDeviceToken(token)
	device.TokenExpiresAt = claims.ExpiresAt

	if err := s.devices.Create(ctx, device); err != nil {
		return RegisterResult{}, err
	}

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: in.UserID, Action: domain.ActionDeviceRegister,
		TargetType: "device", TargetID: device.ID,
		Metadata:  map[string]any{"platform": in.Platform, "agent_version": in.AgentVersion},
		IPAddress: in.IPAddress, UserAgent: in.UserAgent,
		CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: now,
	})
	s.log.Info("device registered",
		slog.String("device_id", device.ID), slog.String("user_id", in.UserID),
		slog.String("platform", in.Platform))

	return RegisterResult{Device: device, Token: token, ExpiresAt: claims.ExpiresAt}, nil
}

// DeviceView combines a device with its live status for the dashboard.
type DeviceView struct {
	Device        domain.Device
	Online        bool
	Status        domain.AgentStatus
	HasStatus     bool
	QueuedPrompts int
}

// List returns a user's devices with presence and status attached.
//
// Presence and status are fetched in two bulk calls rather than per device, so the
// dashboard's most frequent request does not scale with the number of machines a
// user owns.
func (s *DeviceService) List(ctx context.Context, userID string) ([]DeviceView, error) {
	devices, err := s.devices.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	online, err := s.presence.OnlineForUser(ctx, userID)
	if err != nil {
		// Presence is best-effort. Failing the whole listing because Redis is
		// unavailable would take the dashboard down over a non-fatal condition.
		s.log.Warn("presence lookup failed; reporting devices as offline", blog.Err(err))
		online = map[string]bool{}
	}

	statuses, err := s.devices.StatusForUser(ctx, userID)
	if err != nil {
		s.log.Warn("status lookup failed", blog.Err(err))
		statuses = map[string]domain.AgentStatus{}
	}

	out := make([]DeviceView, 0, len(devices))
	for _, d := range devices {
		view := DeviceView{Device: d, Online: online[d.ID]}
		if st, ok := statuses[d.ID]; ok {
			view.Status, view.HasStatus = st, true
			view.QueuedPrompts = st.QueuedPrompts
		}
		out = append(out, view)
	}
	return out, nil
}

// Get returns one device with its status.
func (s *DeviceService) Get(ctx context.Context, deviceID, userID string) (DeviceView, error) {
	device, err := s.devices.ByIDForUser(ctx, deviceID, userID)
	if err != nil {
		return DeviceView{}, err
	}

	view := DeviceView{Device: device}
	if online, err := s.presence.IsOnline(ctx, deviceID); err == nil {
		view.Online = online
	}
	if st, err := s.devices.Status(ctx, deviceID); err == nil {
		view.Status, view.HasStatus = st, true
	}
	if n, err := s.prompts.CountPending(ctx, deviceID); err == nil {
		view.QueuedPrompts = n
	}
	return view, nil
}

// Rename updates a device's display name.
func (s *DeviceService) Rename(ctx context.Context, deviceID, userID, name string) (domain.Device, error) {
	device, err := s.devices.ByIDForUser(ctx, deviceID, userID)
	if err != nil {
		return domain.Device{}, err
	}
	device.Name = name
	if err := s.devices.Update(ctx, device); err != nil {
		return domain.Device{}, err
	}
	return device, nil
}

// Revoke invalidates a device's credentials and disconnects it.
//
// Order matters: revoke in the database first, then close the socket. Closing
// first would leave a window in which the agent reconnects with a token that is
// still valid, and the revocation would appear not to have worked.
func (s *DeviceService) Revoke(ctx context.Context, deviceID, userID, ipAddress string) error {
	now := s.clock.Now()

	if err := s.devices.Revoke(ctx, deviceID, userID, now); err != nil {
		return err
	}

	s.conns.CloseDevice(deviceID, "device revoked")
	if err := s.presence.MarkOffline(ctx, userID, deviceID); err != nil {
		s.log.Warn("failed to clear presence after revoke", blog.Err(err))
	}

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: userID, Action: domain.ActionDeviceRevoked,
		TargetType: "device", TargetID: deviceID,
		IPAddress: ipAddress, CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: now,
	})
	s.log.Info("device revoked", slog.String("device_id", deviceID), slog.String("user_id", userID))
	return nil
}

// Delete removes a device from the user's list.
func (s *DeviceService) Delete(ctx context.Context, deviceID, userID, ipAddress string) error {
	now := s.clock.Now()

	if err := s.devices.SoftDelete(ctx, deviceID, userID, now); err != nil {
		return err
	}
	s.conns.CloseDevice(deviceID, "device deleted")
	_ = s.presence.MarkOffline(ctx, userID, deviceID)

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: userID, Action: domain.ActionDeviceDeleted,
		TargetType: "device", TargetID: deviceID,
		IPAddress: ipAddress, CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: now,
	})
	return nil
}

// AuthenticateDevice validates a device token for the WebSocket handshake.
//
// Four checks, all required, and the order is chosen so the cheapest rejections
// happen first: signature, kind, identity match, then the database lookup that
// covers revocation and expiry. Verify alone is not enough — it cannot know a
// device was revoked five minutes ago.
func (s *DeviceService) AuthenticateDevice(ctx context.Context, token, claimedDeviceID string) (domain.Device, error) {
	claims, err := s.verifier.Verify(token)
	if err != nil {
		return domain.Device{}, err
	}
	if claims.Kind != domain.KindDevice {
		return domain.Device{}, fmt.Errorf("%w: not a device token", domain.ErrUnauthorized)
	}
	// The device ID in the AUTH payload must match the one the token was minted
	// for, so a valid token cannot be used to impersonate a different device.
	if claims.DeviceID != claimedDeviceID {
		return domain.Device{}, fmt.Errorf("%w: device id mismatch", domain.ErrUnauthorized)
	}

	device, err := s.devices.ByID(ctx, claims.DeviceID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Device{}, fmt.Errorf("%w: unknown device", domain.ErrUnauthorized)
		}
		return domain.Device{}, err
	}
	if device.UserID != claims.Subject {
		return domain.Device{}, fmt.Errorf("%w: device ownership mismatch", domain.ErrUnauthorized)
	}
	if device.Revoked() {
		return domain.Device{}, domain.ErrDeviceRevoked
	}
	if !device.TokenValid(s.clock.Now()) {
		return domain.Device{}, domain.ErrTokenExpired
	}

	// Constant-time comparison against the stored hash. This catches a token that
	// verifies but was superseded by a re-registration, and avoids leaking the
	// stored hash through response timing.
	if !auth.ConstantTimeEqual(device.TokenHash, auth.HashDeviceToken(token)) {
		return domain.Device{}, fmt.Errorf("%w: token does not match this device", domain.ErrUnauthorized)
	}
	return device, nil
}

// UpdateFromHandshake records what the AUTH frame reported about the device.
//
// Capabilities and agent version are self-reported and change when the user
// installs a coding agent or upgrades the binary, so they are refreshed on every
// connection rather than only at registration. Nothing security-relevant is taken
// from the handshake: the token and ownership were already verified.
func (s *DeviceService) UpdateFromHandshake(ctx context.Context, device domain.Device) error {
	return s.devices.Update(ctx, device)
}

// MarkOnline records a device connection.
func (s *DeviceService) MarkOnline(ctx context.Context, device domain.Device) error {
	now := s.clock.Now()
	if err := s.devices.TouchLastSeen(ctx, device.ID, now); err != nil {
		s.log.Warn("failed to record last_seen", blog.Err(err))
	}
	// TTL is 2.5x the heartbeat interval, matching the protocol's own tolerance:
	// a single missed heartbeat must not make a healthy device appear offline.
	return s.presence.MarkOnline(ctx, device.UserID, device.ID, protocol.HeartbeatTimeout)
}

// MarkOffline clears presence on disconnect.
func (s *DeviceService) MarkOffline(ctx context.Context, userID, deviceID string) error {
	return s.presence.MarkOffline(ctx, userID, deviceID)
}

// RecordStatus persists a STATUS frame from an agent.
func (s *DeviceService) RecordStatus(ctx context.Context, deviceID string, p protocol.StatusPayload, sessionID string, at time.Time) error {
	queued, err := s.prompts.CountPending(ctx, deviceID)
	if err != nil {
		queued = p.QueuedPrompts // fall back to what the agent reported
	}
	return s.devices.SaveStatus(ctx, domain.AgentStatus{
		DeviceID:      deviceID,
		State:         string(p.State),
		Adapter:       p.Adapter,
		Repository:    p.Repository,
		CurrentTask:   p.CurrentTask,
		CPUPercent:    p.CPUPercent,
		MemoryBytes:   p.MemoryBytes,
		QueuedPrompts: queued,
		SessionID:     sessionID,
		ReportedAt:    at,
	})
}

// Touch refreshes liveness on a heartbeat.
func (s *DeviceService) Touch(ctx context.Context, device domain.Device) error {
	now := s.clock.Now()
	if err := s.devices.TouchLastSeen(ctx, device.ID, now); err != nil {
		return err
	}
	return s.presence.MarkOnline(ctx, device.UserID, device.ID, protocol.HeartbeatTimeout)
}
