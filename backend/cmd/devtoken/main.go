// Command devtoken mints a local user and access token for development.
//
// GitHub OAuth needs a registered OAuth app and a browser round trip. That is the
// right flow for real users and a poor one for local development, integration
// tests, and the dashboard's dev server — all of which need a valid access token
// and none of which should require cloud credentials to run.
//
// This is a development affordance and is guarded accordingly: it refuses to run
// unless BEUVIAN_ENV is development, and it never touches production data because
// it cannot start against a production configuration.
//
//	go run ./cmd/devtoken                      # mint a token for the default dev user
//	go run ./cmd/devtoken -login alice         # a distinct user
//
// The token is printed to stdout and nothing else, so it can be captured directly:
//
//	TOKEN=$(go run ./cmd/devtoken)
package main

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/internal/adapter/auth"
	"github.com/bhuvan0808/beuviancode/backend/internal/adapter/postgres"
	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/shared/id"
)

func main() {
	os.Exit(run())
}

func run() int {
	login := flag.String("login", "devuser", "GitHub login to create or reuse")
	ttl := flag.Duration("ttl", time.Hour, "access token lifetime")
	verbose := flag.Bool("v", false, "print details to stderr as well as the token")
	flag.Parse()

	cfg, _, _, err := config.Load(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}

	// The guard. This tool mints credentials without any authentication, so it
	// must be impossible to point at a real deployment. Checking the resolved
	// configuration rather than a bare environment variable means it also refuses
	// when production settings arrive from a config file.
	if !cfg.Env.IsDevelopment() {
		fmt.Fprintf(os.Stderr,
			"devtoken refuses to run with env=%q; it is a development-only tool\n", cfg.Env)
		return 2
	}
	if cfg.Database.URL == "" {
		fmt.Fprintln(os.Stderr, "no database configured; set BEUVIAN_DB_URL")
		return 2
	}
	if cfg.Auth.JWTSecret == "" {
		fmt.Fprintln(os.Stderr, "no signing secret configured; set BEUVIAN_AUTH_JWT_SECRET")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 1
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "database unreachable:", err)
		return 1
	}

	// A stable synthetic GitHub ID derived from the login, so re-running with the
	// same login reuses the same account rather than creating a new one each time.
	// That matters: a test that accumulates users every run eventually behaves
	// differently from a fresh one.
	githubID := int64(hashLogin(*login))

	users := postgres.NewUserStore(pool)
	user, err := users.UpsertByGitHub(ctx,
		domain.User{
			ID:          id.WithPrefix(id.PrefixUser),
			GitHubID:    githubID,
			GitHubLogin: *login,
			Name:        *login,
			LastLoginAt: time.Now().UTC(),
		},
		domain.OAuthAccount{
			ID:             id.WithPrefix("oap"),
			Provider:       domain.ProviderGitHub,
			ProviderUserID: fmt.Sprint(githubID),
			Scopes:         []string{"read:user"},
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create user:", err)
		return 1
	}

	authCfg := cfg.Auth
	authCfg.AccessTokenTTL = *ttl
	token, claims, err := auth.NewTokenService(authCfg).IssueAccess(user.ID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue token:", err)
		return 1
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "user:    %s (%s)\n", user.GitHubLogin, user.ID)
		fmt.Fprintf(os.Stderr, "expires: %s\n", claims.ExpiresAt.Format(time.RFC1123))
	}

	// The token alone on stdout, so it can be captured with $(...).
	fmt.Println(token)
	return 0
}

// hashLogin derives a stable synthetic GitHub ID from a login.
func hashLogin(login string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(login))
	// Bounded well away from real GitHub IDs to make a collision with imported
	// production data implausible.
	return h.Sum32()%1_000_000 + 900_000_000
}
