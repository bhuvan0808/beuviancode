package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"

	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/version"
)

// GitHub implements port.OAuthProvider.
type GitHub struct {
	cfg    *oauth2.Config
	client *http.Client
}

// NewGitHub builds a GitHub OAuth provider.
//
// Scopes are deliberately minimal:
//
//	read:user  — the identity needed to create an account
//	repo       — repository metadata, needed to list private repositories
//
// Beuvian only ever READS repository metadata; it never writes to a user's GitHub
// account. Requesting more than this would be asking users to grant access the
// product does not use, which is both a trust and a blast-radius problem.
func NewGitHub(cfg config.Auth) *GitHub {
	return &GitHub{
		cfg: &oauth2.Config{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
			RedirectURL:  cfg.GitHubCallbackURL,
			Scopes:       []string{"read:user", "user:email", "repo"},
			Endpoint:     github.Endpoint,
		},
		// An explicit client with a timeout. http.DefaultClient has none, so a
		// hung GitHub request would hold a backend goroutine and a connection
		// indefinitely.
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

var _ port.OAuthProvider = (*GitHub)(nil)

// AuthCodeURL builds the GitHub redirect, embedding the CSRF state.
func (g *GitHub) AuthCodeURL(state string) string {
	return g.cfg.AuthCodeURL(state, oauth2.AccessTypeOnline)
}

// Exchange trades an authorization code for an access token.
func (g *GitHub) Exchange(ctx context.Context, code string) (string, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.client)

	tok, err := g.cfg.Exchange(ctx, code)
	if err != nil {
		return "", fmt.Errorf("auth: github code exchange failed: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("auth: github returned an empty access token")
	}
	return tok.AccessToken, nil
}

// githubUserResponse mirrors the fields we use from GET /user.
type githubUserResponse struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// githubEmailResponse mirrors GET /user/emails.
type githubEmailResponse struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// CurrentUser reads the authenticated identity.
//
// Falls back to /user/emails when the profile email is hidden, which is the
// default for a large share of GitHub accounts. Without the fallback those users
// would end up with no email at all — acceptable for login, but it would silently
// break any future email-based feature.
func (g *GitHub) CurrentUser(ctx context.Context, accessToken string) (port.GitHubUser, error) {
	var u githubUserResponse
	if err := g.get(ctx, accessToken, "https://api.github.com/user", &u); err != nil {
		return port.GitHubUser{}, err
	}
	if u.ID == 0 {
		return port.GitHubUser{}, fmt.Errorf("auth: github returned a user with no id")
	}

	out := port.GitHubUser{
		ID:        u.ID,
		Login:     u.Login,
		Name:      u.Name,
		Email:     u.Email,
		AvatarURL: u.AvatarURL,
	}

	if out.Email == "" {
		var emails []githubEmailResponse
		// A failure here is not fatal: the account is identified by its numeric
		// ID, and an email is a nicety rather than a requirement for login.
		if err := g.get(ctx, accessToken, "https://api.github.com/user/emails", &emails); err == nil {
			for _, e := range emails {
				if e.Primary && e.Verified {
					out.Email = e.Email
					break
				}
			}
		}
	}
	return out, nil
}

// githubRepoResponse mirrors the fields we use from GET /user/repos.
type githubRepoResponse struct {
	ID            int64     `json:"id"`
	FullName      string    `json:"full_name"`
	DefaultBranch string    `json:"default_branch"`
	Private       bool      `json:"private"`
	Description   string    `json:"description"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ListRepos reads repository metadata, most recently updated first.
//
// Sorted by update time and capped, because a user with hundreds of repositories
// does not want to scroll to find the one they are working on today — and because
// paging through all of them would burn their GitHub rate limit, which is per-user
// and shared with everything else they do.
func (g *GitHub) ListRepos(ctx context.Context, accessToken string, limit int) ([]port.GitHubRepo, error) {
	if limit <= 0 || limit > 100 {
		limit = 100 // GitHub's per-page maximum
	}
	url := fmt.Sprintf(
		"https://api.github.com/user/repos?sort=updated&direction=desc&per_page=%d&affiliation=owner,collaborator",
		limit)

	var repos []githubRepoResponse
	if err := g.get(ctx, accessToken, url, &repos); err != nil {
		return nil, err
	}

	out := make([]port.GitHubRepo, 0, len(repos))
	for _, r := range repos {
		out = append(out, port.GitHubRepo{
			ID:            r.ID,
			FullName:      r.FullName,
			DefaultBranch: r.DefaultBranch,
			Private:       r.Private,
			Description:   r.Description,
			UpdatedAt:     r.UpdatedAt,
		})
	}
	return out, nil
}

// get performs an authenticated GitHub API request.
func (g *GitHub) get(ctx context.Context, accessToken, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("auth: build github request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// Identifying ourselves is required by GitHub and makes our traffic
	// attributable if we ever misbehave.
	req.Header.Set("User-Agent", version.UserAgent("backend"))

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: github request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// The stored OAuth token was revoked by the user on GitHub's side. Worth
		// distinguishing so the caller can prompt a re-login rather than retrying.
		return fmt.Errorf("auth: github rejected the access token")
	case resp.StatusCode == http.StatusForbidden:
		// GitHub returns 403 for rate limiting as well as permissions.
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return fmt.Errorf("auth: github rate limit exceeded; resets at %s",
				resp.Header.Get("X-RateLimit-Reset"))
		}
		return fmt.Errorf("auth: github denied the request")
	case resp.StatusCode >= 400:
		return fmt.Errorf("auth: github returned %s", resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("auth: decode github response: %w", err)
	}
	return nil
}
