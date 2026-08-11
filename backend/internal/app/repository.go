package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
)

// RepositoryService manages repositories and GitHub metadata.
type RepositoryService struct {
	repos port.RepositoryStore
	users port.UserStore
	oauth port.OAuthProvider
	cache port.Cache
	ids   port.IDGenerator
	clock port.Clock
	log   *slog.Logger
}

// RepositoryDeps groups RepositoryService's collaborators.
type RepositoryDeps struct {
	Repos port.RepositoryStore
	Users port.UserStore
	OAuth port.OAuthProvider
	Cache port.Cache
	IDs   port.IDGenerator
	Clock port.Clock
	Log   *slog.Logger
}

// NewRepositoryService builds a RepositoryService.
func NewRepositoryService(d RepositoryDeps) *RepositoryService {
	return &RepositoryService{
		repos: d.Repos, users: d.Users, oauth: d.OAuth, cache: d.Cache,
		ids: d.IDs, clock: d.Clock,
		log: d.Log.With(slog.String("service", "repository")),
	}
}

// AddInput describes a repository registration.
type AddInput struct {
	UserID        string
	FullName      string
	LocalPath     string
	DeviceID      string
	DefaultBranch string
	GitHubID      int64
	IsPrivate     bool
}

// Add registers a repository.
func (s *RepositoryService) Add(ctx context.Context, in AddInput) (domain.Repository, error) {
	now := s.clock.Now()
	repo := domain.Repository{
		ID:            s.ids.NewID(id.PrefixRepository),
		UserID:        in.UserID,
		FullName:      in.FullName,
		LocalPath:     in.LocalPath,
		DeviceID:      in.DeviceID,
		DefaultBranch: in.DefaultBranch,
		GitHubID:      in.GitHubID,
		IsPrivate:     in.IsPrivate,
		CreatedAt:     now,
	}
	if err := repo.Validate(); err != nil {
		return domain.Repository{}, err
	}
	if err := s.repos.Create(ctx, repo); err != nil {
		return domain.Repository{}, err
	}
	return repo, nil
}

// List returns a user's repositories.
func (s *RepositoryService) List(ctx context.Context, userID string) ([]domain.Repository, error) {
	return s.repos.ListForUser(ctx, userID)
}

// Get returns one repository.
func (s *RepositoryService) Get(ctx context.Context, repoID, userID string) (domain.Repository, error) {
	return s.repos.ByIDForUser(ctx, repoID, userID)
}

// UpdateInput describes a repository update.
type UpdateInput struct {
	LocalPath     *string
	DeviceID      *string
	DefaultBranch *string
}

// Update changes a repository's local location.
//
// Pointer fields distinguish "not supplied" from "set to empty". Without that, a
// PATCH omitting local_path would clear it, which is a data-loss bug that reads as
// working correctly in the common case where all fields are sent.
func (s *RepositoryService) Update(ctx context.Context, repoID, userID string, in UpdateInput) (domain.Repository, error) {
	repo, err := s.repos.ByIDForUser(ctx, repoID, userID)
	if err != nil {
		return domain.Repository{}, err
	}
	if in.LocalPath != nil {
		repo.LocalPath = *in.LocalPath
	}
	if in.DeviceID != nil {
		repo.DeviceID = *in.DeviceID
	}
	if in.DefaultBranch != nil {
		repo.DefaultBranch = *in.DefaultBranch
	}
	if err := repo.Validate(); err != nil {
		return domain.Repository{}, err
	}
	if err := s.repos.Update(ctx, repo); err != nil {
		return domain.Repository{}, err
	}
	return repo, nil
}

// Delete removes a repository from the user's list.
func (s *RepositoryService) Delete(ctx context.Context, repoID, userID string) error {
	return s.repos.SoftDelete(ctx, repoID, userID, s.clock.Now())
}

// githubCacheTTL bounds how long GitHub repository listings are reused.
//
// Five minutes is a deliberate compromise. GitHub's rate limit is per user and
// shared with everything else they do, so an uncached dashboard refresh loop would
// exhaust it. Five minutes is short enough that a newly created repository appears
// promptly and long enough that ordinary navigation costs one upstream call.
const githubCacheTTL = 5 * time.Minute

// ListGitHub returns the user's GitHub repositories, cached briefly.
func (s *RepositoryService) ListGitHub(ctx context.Context, userID string) ([]port.GitHubRepo, error) {
	cacheKey := "github:repos:" + userID

	if cached, err := s.cache.Get(ctx, cacheKey); err == nil {
		var repos []port.GitHubRepo
		if json.Unmarshal(cached, &repos) == nil {
			return repos, nil
		}
		// A corrupt entry is not worth failing over; fall through and refetch.
	}

	account, err := s.users.OAuthAccount(ctx, userID, domain.ProviderGitHub)
	if err != nil {
		return nil, fmt.Errorf("%w: no linked GitHub account", domain.ErrForbidden)
	}

	repos, err := s.oauth.ListRepos(ctx, account.AccessTokenEncrypted, 100)
	if err != nil {
		return nil, err
	}

	if body, err := json.Marshal(repos); err == nil {
		if err := s.cache.Set(ctx, cacheKey, body, githubCacheTTL); err != nil {
			// Caching is an optimisation; a failure only costs the next caller a
			// round trip.
			s.log.Debug("failed to cache github repos", blog.Err(err))
		}
	}
	return repos, nil
}

// SettingsService reads and writes user preferences.
type SettingsService struct {
	users port.UserStore
	audit port.AuditLogger
	clock port.Clock
}

// NewSettingsService builds a SettingsService.
func NewSettingsService(users port.UserStore, audit port.AuditLogger, clock port.Clock) *SettingsService {
	return &SettingsService{users: users, audit: audit, clock: clock}
}

// Get returns a user's settings.
func (s *SettingsService) Get(ctx context.Context, userID string) (domain.UserSettings, error) {
	return s.users.Settings(ctx, userID)
}

// SettingsPatch describes a partial settings update.
//
// Pointer fields for the same reason as UpdateInput: a PATCH that omits a boolean
// must leave it alone rather than setting it false.
type SettingsPatch struct {
	NotifyOnComplete      *bool
	NotifyOnWaiting       *bool
	NotifyOnDeviceOffline *bool
	Theme                 *string
	Timezone              *string
	LogRetentionDays      *int
}

// Update applies a settings patch.
func (s *SettingsService) Update(ctx context.Context, userID string, p SettingsPatch) (domain.UserSettings, error) {
	current, err := s.users.Settings(ctx, userID)
	if err != nil {
		return domain.UserSettings{}, err
	}

	if p.NotifyOnComplete != nil {
		current.NotifyOnComplete = *p.NotifyOnComplete
	}
	if p.NotifyOnWaiting != nil {
		current.NotifyOnWaiting = *p.NotifyOnWaiting
	}
	if p.NotifyOnDeviceOffline != nil {
		current.NotifyOnDeviceOffline = *p.NotifyOnDeviceOffline
	}
	if p.Theme != nil {
		current.Theme = *p.Theme
	}
	if p.Timezone != nil {
		current.Timezone = *p.Timezone
	}
	if p.LogRetentionDays != nil {
		current.LogRetentionDays = *p.LogRetentionDays
	}

	if err := current.Validate(); err != nil {
		return domain.UserSettings{}, err
	}
	if err := s.users.SaveSettings(ctx, current); err != nil {
		return domain.UserSettings{}, err
	}

	s.audit.Record(ctx, domain.AuditEntry{
		UserID: userID, Action: domain.ActionSettingsChanged,
		TargetType: "user_settings", TargetID: userID,
		CorrelationID: blog.CorrelationIDFrom(ctx), CreatedAt: s.clock.Now(),
	})
	return current, nil
}
