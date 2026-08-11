package domain

import (
	"strings"
	"time"
)

// User is a Beuvian account.
//
// Identity is anchored on the GitHub numeric ID, never the login, because logins
// are renameable. Keying on the login would make a user who renames their GitHub
// account into a different person, losing their devices and history.
type User struct {
	ID          string
	GitHubID    int64
	GitHubLogin string
	Email       string // may be empty: GitHub users can hide it
	Name        string
	AvatarURL   string

	// OrganizationID is unused in the MVP. Present from the start so enabling
	// teams later is a constraint change rather than a backfill of every row.
	OrganizationID string

	LastLoginAt time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Validate checks the invariants that must hold for any persisted user.
func (u *User) Validate() error {
	if u.GitHubID == 0 {
		return Invalid("github_id", "must be set")
	}
	if strings.TrimSpace(u.GitHubLogin) == "" {
		return Invalid("github_login", "must not be empty")
	}
	return nil
}

// Active reports whether the account is usable.
func (u *User) Active() bool { return u.DeletedAt == nil }

// DisplayName returns the best available human-readable name.
func (u *User) DisplayName() string {
	if strings.TrimSpace(u.Name) != "" {
		return u.Name
	}
	return u.GitHubLogin
}

// OAuthProvider names an identity provider.
type OAuthProvider string

// ProviderGitHub is the only provider in the MVP. Modelled as a type rather than
// a bare string so adding a provider is a new constant, not a new column.
const ProviderGitHub OAuthProvider = "github"

// OAuthAccount links a User to an external identity.
//
// Separate from User so a second provider is a new row rather than a schema
// change.
type OAuthAccount struct {
	ID             string
	UserID         string
	Provider       OAuthProvider
	ProviderUserID string

	// AccessTokenEncrypted is the provider token, encrypted at rest. Beuvian
	// uses it only to read repository metadata. It is never a model-provider
	// credential — Beuvian holds none of those.
	AccessTokenEncrypted string
	Scopes               []string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// UserSettings holds per-user preferences.
//
// Created on first login so reads never have to handle absence, which removes a
// nil check from every notification decision.
type UserSettings struct {
	UserID                string
	NotifyOnComplete      bool
	NotifyOnWaiting       bool
	NotifyOnDeviceOffline bool
	Theme                 string
	Timezone              string
	LogRetentionDays      int
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// DefaultSettings returns the settings a new account starts with.
//
// Completion and waiting notifications default ON because they are the product's
// entire purpose. Device-offline defaults OFF: laptops disconnect constantly and
// notifying every time would train the user to ignore all notifications.
func DefaultSettings(userID string) UserSettings {
	return UserSettings{
		UserID:                userID,
		NotifyOnComplete:      true,
		NotifyOnWaiting:       true,
		NotifyOnDeviceOffline: false,
		Theme:                 "system",
		Timezone:              "UTC",
		LogRetentionDays:      30,
	}
}

// Validate checks settings invariants.
func (s *UserSettings) Validate() error {
	if s.UserID == "" {
		return Invalid("user_id", "must be set")
	}
	if s.LogRetentionDays < 1 || s.LogRetentionDays > 365 {
		return Invalid("log_retention_days", "must be between 1 and 365")
	}
	switch s.Theme {
	case "system", "light", "dark":
	default:
		return Invalid("theme", "must be system, light or dark")
	}
	return nil
}
