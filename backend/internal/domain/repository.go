package domain

import (
	"regexp"
	"strings"
	"time"
)

// Repository is a code repository a user works on.
type Repository struct {
	ID     string
	UserID string

	// FullName is "owner/name".
	FullName string

	// LocalPath is the directory on a specific device. Empty when the repository
	// is known from GitHub but not yet located on any machine.
	LocalPath string
	DeviceID  string

	DefaultBranch string
	GitHubID      int64
	IsPrivate     bool

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// fullNamePattern matches GitHub's "owner/name" form.
//
// Validated rather than accepted freely because FullName is rendered in the
// dashboard and used to build GitHub URLs; an unvalidated value is both a display
// and an injection concern.
var fullNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// Validate checks repository invariants.
func (r *Repository) Validate() error {
	if r.UserID == "" {
		return Invalid("user_id", "must be set")
	}
	name := strings.TrimSpace(r.FullName)
	if name == "" {
		return Invalid("full_name", "must not be empty")
	}
	if !fullNamePattern.MatchString(name) {
		return Invalid("full_name", `must be in "owner/name" form`)
	}
	// A local path without a device is meaningless: the same path on a different
	// machine is a different directory.
	if r.LocalPath != "" && r.DeviceID == "" {
		return Invalid("device_id", "must be set when local_path is set")
	}
	return nil
}

// Owner returns the owner segment of FullName.
func (r *Repository) Owner() string {
	if i := strings.Index(r.FullName, "/"); i > 0 {
		return r.FullName[:i]
	}
	return ""
}

// Name returns the repository segment of FullName.
func (r *Repository) Name() string {
	if i := strings.Index(r.FullName, "/"); i >= 0 && i+1 < len(r.FullName) {
		return r.FullName[i+1:]
	}
	return r.FullName
}

// Located reports whether the repository is available on a device.
func (r *Repository) Located() bool { return r.LocalPath != "" && r.DeviceID != "" }
