package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/bhuvan0808/beuviancode/agent/internal/config"
	"github.com/bhuvan0808/beuviancode/agent/internal/store"
	"github.com/bhuvan0808/beuviancode/shared/retry"
	"github.com/bhuvan0808/beuviancode/shared/version"
)

// Registrar registers this installation with the backend.
//
// Registration is REST rather than WebSocket for a structural reason: opening the
// socket requires a device token, and this is how that token is obtained. It is
// the one operation that uses the USER's access token, and the only point where
// the two credential families touch.
type Registrar struct {
	cfg    config.Backend
	client *http.Client
}

// NewRegistrar builds a Registrar.
func NewRegistrar(cfg config.Backend) *Registrar {
	return &Registrar{
		cfg: cfg,
		// An explicit timeout: http.DefaultClient has none, so a hung backend
		// would block registration forever with no feedback to the user.
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// RegisterRequest is the registration payload.
type RegisterRequest struct {
	Name         string   `json:"name"`
	Platform     string   `json:"platform"`
	AgentVersion string   `json:"agent_version"`
	Capabilities []string `json:"capabilities"`
}

// RegisterResponse mirrors the backend's reply.
type RegisterResponse struct {
	Device struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"device"`
	DeviceToken string    `json:"device_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// ErrRegistrationRejected means the backend refused the registration.
//
// Distinguished from a transport failure because the responses differ: a rejected
// registration needs a new access token from the user, while a network failure
// just needs retrying.
var ErrRegistrationRejected = errors.New("transport: registration rejected")

// Register exchanges a user access token for a device token.
//
// The returned token is stored and never retrievable again — the backend keeps
// only its hash — so the caller must persist it before doing anything else.
func (r *Registrar) Register(ctx context.Context, accessToken string, capabilities []string) (RegisterResponse, error) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-device"
	}
	if capabilities == nil {
		capabilities = []string{}
	}

	body, err := json.Marshal(RegisterRequest{
		Name:         hostname,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		AgentVersion: version.Get().Version,
		Capabilities: capabilities,
	})
	if err != nil {
		return RegisterResponse{}, err
	}

	var out RegisterResponse
	// Registration is worth retrying: a user running this at a coffee shop should
	// not have to re-run the command because of one dropped packet. Bounded, so a
	// genuinely wrong URL fails promptly rather than hanging.
	err = retry.Do(ctx, retry.DefaultPolicy(), func(ctx context.Context) error {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost,
			r.cfg.APIURL+"/v1/devices/register", bytes.NewReader(body))
		if rerr != nil {
			return retry.Fatal(rerr)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", version.UserAgent("agent"))

		resp, rerr := r.client.Do(req)
		if rerr != nil {
			return rerr // network failure: retryable
		}
		defer func() { _ = resp.Body.Close() }()

		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

		switch {
		case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
			if jerr := json.Unmarshal(raw, &out); jerr != nil {
				return retry.Fatal(fmt.Errorf("decode registration response: %w", jerr))
			}
			if out.DeviceToken == "" || out.Device.ID == "" {
				return retry.Fatal(fmt.Errorf("%w: backend returned an incomplete registration", ErrRegistrationRejected))
			}
			return nil

		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
			// A bad access token will not fix itself. Retrying wastes the user's
			// time and hammers the auth endpoint.
			return retry.Fatal(fmt.Errorf("%w: the access token was rejected (status %d)",
				ErrRegistrationRejected, resp.StatusCode))

		case resp.StatusCode >= 500:
			return fmt.Errorf("backend error %s", resp.Status) // retryable

		default:
			return retry.Fatal(fmt.Errorf("%w: %s: %s",
				ErrRegistrationRejected, resp.Status, truncate(string(raw), 200)))
		}
	})
	if err != nil {
		return RegisterResponse{}, err
	}
	return out, nil
}

// SaveRegistration persists the credentials returned by Register.
//
// Separate from Register so the network call and the disk write can be reasoned
// about independently: a failure here means the token exists on the backend but
// not locally, which the caller reports as needing a re-run rather than silently
// losing it.
func SaveRegistration(s *store.Store, resp RegisterResponse, userID string) error {
	return s.Update(func(st *store.State) {
		st.DeviceID = resp.Device.ID
		st.DeviceToken = resp.DeviceToken
		st.TokenExpiry = resp.ExpiresAt
		st.UserID = userID
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
