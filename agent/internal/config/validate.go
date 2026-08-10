package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Validate checks the resolved agent configuration.
//
// Like the backend's validator, every problem is collected and reported together.
// This matters more here than on the server: the person reading the error is a
// developer on their own laptop with no logs pipeline, and one error at a time
// turns setup into a guessing game.
func (c *Config) Validate() error {
	var errs []error

	errs = append(errs, c.Backend.validate()...)
	errs = append(errs, c.Device.validate()...)
	errs = append(errs, c.Coding.validate()...)
	errs = append(errs, c.Session.validate()...)
	errs = append(errs, c.Queue.validate()...)
	errs = append(errs, c.Log.validate()...)

	if len(errs) > 0 {
		return fmt.Errorf("invalid agent configuration:\n%w", errors.Join(errs...))
	}
	return nil
}

func (b Backend) validate() []error {
	var errs []error

	u, err := url.Parse(b.URL)
	switch {
	case b.URL == "":
		errs = append(errs, errors.New("backend.url: required"))
	case err != nil:
		errs = append(errs, fmt.Errorf("backend.url: not a valid URL: %w", err))
	case u.Scheme != "ws" && u.Scheme != "wss":
		errs = append(errs, fmt.Errorf(
			"backend.url: scheme %q must be ws or wss (got %q)", u.Scheme, b.URL))
	case u.Host == "":
		errs = append(errs, fmt.Errorf("backend.url: missing host in %q", b.URL))
	}

	if b.APIURL == "" {
		errs = append(errs, errors.New("backend.api_url: required"))
	} else if au, aerr := url.Parse(b.APIURL); aerr != nil {
		errs = append(errs, fmt.Errorf("backend.api_url: not a valid URL: %w", aerr))
	} else if au.Scheme != "http" && au.Scheme != "https" {
		errs = append(errs, fmt.Errorf("backend.api_url: scheme %q must be http or https", au.Scheme))
	}

	// Skipping TLS verification against a real wss:// endpoint is
	// indistinguishable from accepting a machine-in-the-middle. The escape hatch
	// exists for local self-signed testing over ws://, and only there.
	if b.InsecureSkipVerify && err == nil && u != nil && u.Scheme == "wss" {
		errs = append(errs, errors.New(
			"backend.insecure_skip_verify: refusing to disable TLS verification for a wss:// endpoint; "+
				"this would accept any certificate and defeat transport security"))
	}

	if b.ConnectTimeout <= 0 {
		errs = append(errs, errors.New("backend.connect_timeout: must be positive"))
	}
	return errs
}

func (d Device) validate() []error {
	var errs []error

	if d.Name == "" {
		errs = append(errs, errors.New("device.name: must not be empty"))
	}
	if d.StatePath == "" {
		errs = append(errs, errors.New("device.state_path: must not be empty"))
		return errs
	}

	// The state file holds device tokens, so its directory must be creatable
	// before the first connection rather than failing at the moment credentials
	// need persisting.
	dir := filepath.Dir(d.StatePath)
	if dir != "" && dir != "." {
		if info, err := os.Stat(dir); err == nil {
			if !info.IsDir() {
				errs = append(errs, fmt.Errorf(
					"device.state_path: %q exists but is not a directory", dir))
			}
		} else if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("device.state_path: cannot access %q: %w", dir, err))
		}
		// A non-existent directory is fine; the state store creates it on write.
	}

	// An ID supplied by configuration must look like one we issued, so a typo
	// surfaces here instead of as an authentication failure against the backend.
	if d.ID != "" && !strings.HasPrefix(d.ID, "dev_") {
		errs = append(errs, fmt.Errorf(
			"device.id: %q does not look like a Beuvian device ID (expected a dev_ prefix)", d.ID))
	}
	return errs
}

func (c Coding) validate() []error {
	var errs []error

	if c.Adapter == "" {
		errs = append(errs, errors.New("coding.adapter: required"))
	}

	// WorkingDirectory is optional at startup — the agent can connect, report
	// itself online, and be pointed at a repository from the dashboard later. But
	// if one IS configured it must exist, because the alternative is a coding
	// agent writing files into a path the user did not mean.
	if c.WorkingDirectory != "" {
		info, err := os.Stat(c.WorkingDirectory)
		switch {
		case os.IsNotExist(err):
			errs = append(errs, fmt.Errorf(
				"coding.working_directory: %q does not exist", c.WorkingDirectory))
		case err != nil:
			errs = append(errs, fmt.Errorf(
				"coding.working_directory: cannot access %q: %w", c.WorkingDirectory, err))
		case !info.IsDir():
			errs = append(errs, fmt.Errorf(
				"coding.working_directory: %q is not a directory", c.WorkingDirectory))
		}
	}

	if c.ExecutablePath != "" {
		if info, err := os.Stat(c.ExecutablePath); os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf(
				"coding.executable_path: %q does not exist", c.ExecutablePath))
		} else if err == nil && info.IsDir() {
			errs = append(errs, fmt.Errorf(
				"coding.executable_path: %q is a directory, not an executable", c.ExecutablePath))
		}
	}

	// AutoStart without a directory would fail at the moment of launch, after the
	// user believed startup succeeded. Catch the contradiction at boot.
	if c.AutoStart && c.WorkingDirectory == "" {
		errs = append(errs, errors.New(
			"coding.auto_start requires coding.working_directory; there is nothing to start a session in"))
	}
	return errs
}

func (s Session) validate() []error {
	var errs []error

	if s.LogBufferLines < 1 {
		errs = append(errs, fmt.Errorf("session.log_buffer_lines: %d must be at least 1", s.LogBufferLines))
	}
	if s.LogFlushInterval <= 0 {
		errs = append(errs, errors.New("session.log_flush_interval: must be positive"))
	}
	if s.LogBatchSize < 1 {
		errs = append(errs, fmt.Errorf("session.log_batch_size: %d must be at least 1", s.LogBatchSize))
	}
	if s.MaxLogLineBytes < 256 {
		errs = append(errs, fmt.Errorf(
			"session.max_log_line_bytes: %d is too small to carry a useful line", s.MaxLogLineBytes))
	}
	if s.IdleTimeout <= 0 {
		errs = append(errs, errors.New("session.idle_timeout: must be positive"))
	}
	if s.StatusInterval <= 0 {
		errs = append(errs, errors.New("session.status_interval: must be positive"))
	}

	// A batch larger than the ring buffer could never be filled from it, so the
	// early-flush path would be dead and every flush would wait for the timer.
	if s.LogBatchSize > s.LogBufferLines {
		errs = append(errs, fmt.Errorf(
			"session: log_batch_size (%d) cannot exceed log_buffer_lines (%d)",
			s.LogBatchSize, s.LogBufferLines))
	}
	return errs
}

func (q Queue) validate() []error {
	var errs []error
	if q.MaxOfflinePrompts < 1 {
		errs = append(errs, fmt.Errorf(
			"queue.max_offline_prompts: %d must be at least 1, or prompts sent while offline are lost",
			q.MaxOfflinePrompts))
	}
	if q.MaxOutboundEvents < 1 {
		errs = append(errs, fmt.Errorf("queue.max_outbound_events: %d must be at least 1", q.MaxOutboundEvents))
	}
	return errs
}

func (l Log) validate() []error {
	var errs []error
	switch strings.ToLower(l.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level: %q is not one of debug, info, warn, error", l.Level))
	}
	switch strings.ToLower(l.Format) {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format: %q is not one of json, text", l.Format))
	}
	if l.FilePath != "" {
		dir := filepath.Dir(l.FilePath)
		if info, err := os.Stat(dir); err == nil && !info.IsDir() {
			errs = append(errs, fmt.Errorf("log.file_path: %q is not a directory", dir))
		}
	}
	return errs
}
