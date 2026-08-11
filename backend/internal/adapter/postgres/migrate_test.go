package postgres

import (
	"strings"
	"testing"
)

// These run with no database. The migrator's file handling is where a schema
// change goes missing silently, so it is worth verifying without infrastructure —
// a test that needs Postgres would not run on every push.

func TestEmbeddedMigrationsLoadAndPair(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations were embedded; the //go:embed pattern is not matching")
	}

	for _, m := range migrations {
		if strings.TrimSpace(m.up) == "" {
			t.Errorf("migration %d (%s) has an empty up script", m.version, m.name)
		}
		// A missing down script is not caught until someone needs to roll back,
		// which is the worst possible moment to discover it.
		if strings.TrimSpace(m.down) == "" {
			t.Errorf("migration %d (%s) has no down script", m.version, m.name)
		}
	}
}

func TestMigrationsAreOrderedAndVersionsUnique(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	seen := map[int]string{}
	for i, m := range migrations {
		if prev, dup := seen[m.version]; dup {
			// Two migrations sharing a version means one silently never applies.
			t.Errorf("version %d is used by both %q and %q", m.version, prev, m.name)
		}
		seen[m.version] = m.name

		if i > 0 && migrations[i-1].version >= m.version {
			t.Errorf("migrations are not sorted ascending: %d came before %d",
				migrations[i-1].version, m.version)
		}
	}
}

func TestParseMigrationName(t *testing.T) {
	tests := []struct {
		filename    string
		wantVersion int
		wantName    string
		wantDir     string
		wantErr     bool
	}{
		{"0001_initial_schema.up.sql", 1, "initial_schema", "up", false},
		{"0001_initial_schema.down.sql", 1, "initial_schema", "down", false},
		{"0042_add_teams.up.sql", 42, "add_teams", "up", false},

		// Each of these would otherwise be skipped silently, which is how a
		// schema change goes missing in production.
		{"initial_schema.up.sql", 0, "", "", true},   // no version
		{"0001_initial_schema.sql", 0, "", "", true}, // no direction
		{"abc_initial.up.sql", 0, "", "", true},      // non-numeric version
		{"0001.up.sql", 0, "", "", true},             // no description
	}

	for _, tc := range tests {
		t.Run(tc.filename, func(t *testing.T) {
			version, name, dir, err := parseMigrationName(tc.filename)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected %q to be rejected rather than skipped", tc.filename)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if version != tc.wantVersion || name != tc.wantName || dir != tc.wantDir {
				t.Errorf("got (%d, %q, %q), want (%d, %q, %q)",
					version, name, dir, tc.wantVersion, tc.wantName, tc.wantDir)
			}
		})
	}
}

func TestInitialSchemaCoversEveryTableInTheSpec(t *testing.T) {
	// PROJECT.md names these tables explicitly. A tripwire so a future migration
	// that renames or drops one is a deliberate act rather than an accident.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	var schema string
	for _, m := range migrations {
		schema += m.up
	}

	required := []string{
		"users", "devices", "repositories", "sessions", "session_logs",
		"messages", "notifications", "prompt_queue", "agent_status",
		"user_settings", "oauth_accounts", "refresh_tokens",
	}
	for _, table := range required {
		if !strings.Contains(schema, "CREATE TABLE "+table) {
			t.Errorf("PROJECT.md requires a %q table; no CREATE TABLE found", table)
		}
	}
}

func TestSchemaEnforcesOneActiveSessionPerDevice(t *testing.T) {
	// The application also checks this, but the unique partial index is the real
	// guarantee: two concurrent start requests would both pass an application-level
	// check and both insert. Losing the index would be a silent correctness
	// regression, so assert it exists.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	var schema string
	for _, m := range migrations {
		schema += m.up
	}

	if !strings.Contains(schema, "idx_sessions_one_active_per_device") {
		t.Error("the unique partial index guaranteeing one live session per device is missing")
	}
	if !strings.Contains(schema, "session_logs_unique_seq") {
		t.Error("the unique (session_id, seq) constraint that makes log ingestion idempotent is missing")
	}
}

func TestTimestampsAreTimezoneAware(t *testing.T) {
	// TIMESTAMP without a time zone silently discards the offset, and Beuvian's
	// users span many. Catching a naive column here is far cheaper than debugging
	// an off-by-hours session duration later.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migrations {
		for _, line := range strings.Split(m.up, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "--") {
				continue
			}
			if strings.Contains(trimmed, "TIMESTAMP ") && !strings.Contains(trimmed, "TIMESTAMPTZ") {
				t.Errorf("migration %d uses a naive TIMESTAMP: %s", m.version, trimmed)
			}
		}
	}
}
