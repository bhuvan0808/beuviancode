// Package postgres implements the port store interfaces against PostgreSQL.
package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/migrations"
)

// Migrator applies versioned SQL migrations.
//
// Hand-rolled rather than golang-migrate, for three reasons: it is roughly 100
// lines, it avoids a dependency whose design is CLI-first, and it lets us hold the
// advisory lock ourselves — which is the part that actually matters for a rolling
// deploy.
type Migrator struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewMigrator returns a Migrator.
func NewMigrator(pool *pgxpool.Pool, log *slog.Logger) *Migrator {
	return &Migrator{pool: pool, log: log.With(slog.String("component", "migrate"))}
}

// migration is one versioned step.
type migration struct {
	version int
	name    string
	up      string
	down    string
}

// advisoryLockKey is an arbitrary but fixed key for the migration lock.
//
// PostgreSQL advisory locks are global to the database, so this serialises
// migrations across every instance and every deploy. Without it, a rolling deploy
// where two instances start simultaneously can run the same DDL concurrently and
// deadlock with the schema half-applied.
const advisoryLockKey int64 = 8_675_309

// Up applies all pending migrations.
//
// Safe to call from several instances at once: the advisory lock means one runs
// them and the rest wait, then observe there is nothing to do. It is nonetheless
// intended to run as an explicit deploy step rather than from application boot —
// see backend/migrations/README.md.
func (m *Migrator) Up(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return fmt.Errorf("postgres: no migrations were embedded")
	}

	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire connection: %w", err)
	}
	defer conn.Release()

	// The lock must be held on a single connection for its whole lifetime, which
	// is why we acquire a dedicated one rather than using the pool directly.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("postgres: acquire migration lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
			m.log.Error("failed to release migration lock", slog.String("error", err.Error()))
		}
	}()

	if err := m.ensureVersionTable(ctx, conn.Conn()); err != nil {
		return err
	}

	applied, err := m.appliedVersions(ctx, conn.Conn())
	if err != nil {
		return err
	}

	pending := 0
	for _, mig := range migrations {
		if applied[mig.version] {
			continue
		}
		pending++
		if err := m.apply(ctx, conn.Conn(), mig); err != nil {
			return err
		}
	}

	if pending == 0 {
		m.log.Info("schema is up to date", slog.Int("version", maxVersion(migrations)))
	} else {
		m.log.Info("migrations applied",
			slog.Int("count", pending),
			slog.Int("version", maxVersion(migrations)))
	}
	return nil
}

// apply runs one migration inside a transaction.
//
// Each migration is its own transaction so a failure leaves the schema at a known
// version rather than partially through a step. PostgreSQL supports transactional
// DDL, which is what makes this possible at all — the same approach on MySQL would
// not be safe.
func (m *Migrator) apply(ctx context.Context, conn *pgx.Conn, mig migration) error {
	m.log.Info("applying migration",
		slog.Int("version", mig.version), slog.String("name", mig.name))

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin migration %d: %w", mig.version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, mig.up); err != nil {
		return fmt.Errorf("postgres: migration %d (%s) failed: %w", mig.version, mig.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		mig.version, mig.name); err != nil {
		return fmt.Errorf("postgres: record migration %d: %w", mig.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit migration %d: %w", mig.version, err)
	}
	return nil
}

// Down rolls back the single most recent migration.
//
// Deliberately one step at a time and not a "down to zero" option. A bulk
// rollback on live data is far more dangerous than the bug it is undoing, and
// making it inconvenient is the point.
func (m *Migrator) Down(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("postgres: acquire migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey) }()

	if err := m.ensureVersionTable(ctx, conn.Conn()); err != nil {
		return err
	}

	var version int
	var name string
	err = conn.QueryRow(ctx,
		`SELECT version, name FROM schema_migrations ORDER BY version DESC LIMIT 1`).
		Scan(&version, &name)
	if err != nil {
		if err == pgx.ErrNoRows {
			m.log.Info("nothing to roll back")
			return nil
		}
		return fmt.Errorf("postgres: read current version: %w", err)
	}

	var target *migration
	for i := range migrations {
		if migrations[i].version == version {
			target = &migrations[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("postgres: no embedded migration for applied version %d", version)
	}
	if strings.TrimSpace(target.down) == "" {
		return fmt.Errorf("postgres: migration %d (%s) has no down script", version, name)
	}

	m.log.Warn("rolling back migration",
		slog.Int("version", version), slog.String("name", name))

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin rollback: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, target.down); err != nil {
		return fmt.Errorf("postgres: rollback %d failed: %w", version, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version); err != nil {
		return fmt.Errorf("postgres: unrecord migration %d: %w", version, err)
	}
	return tx.Commit(ctx)
}

// Version returns the highest applied migration version, or 0 if none.
func (m *Migrator) Version(ctx context.Context) (int, error) {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()

	if err := m.ensureVersionTable(ctx, conn.Conn()); err != nil {
		return 0, err
	}
	var version int
	err = conn.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("postgres: read version: %w", err)
	}
	return version, nil
}

func (m *Migrator) ensureVersionTable(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("postgres: create schema_migrations: %w", err)
	}
	return nil
}

func (m *Migrator) appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("postgres: read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// loadMigrations reads and pairs the embedded up/down files.
//
// Filenames must be NNNN_name.up.sql and NNNN_name.down.sql. A malformed name is
// an error rather than being skipped: silently ignoring a migration file is how a
// schema change goes missing in production.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("postgres: read embedded migrations: %w", err)
	}

	byVersion := map[int]*migration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, direction, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}

		body, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("postgres: read %s: %w", e.Name(), err)
		}

		mig, ok := byVersion[version]
		if !ok {
			mig = &migration{version: version, name: name}
			byVersion[version] = mig
		}
		switch direction {
		case "up":
			mig.up = string(body)
		case "down":
			mig.down = string(body)
		}
	}

	out := make([]migration, 0, len(byVersion))
	for _, mig := range byVersion {
		if strings.TrimSpace(mig.up) == "" {
			return nil, fmt.Errorf("postgres: migration %d (%s) has no up script", mig.version, mig.name)
		}
		out = append(out, *mig)
	}
	// Sorted so migrations apply in version order regardless of directory order.
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func parseMigrationName(filename string) (version int, name, direction string, err error) {
	base := strings.TrimSuffix(filename, ".sql")

	var ok bool
	for _, d := range []string{"up", "down"} {
		if strings.HasSuffix(base, "."+d) {
			direction = d
			base = strings.TrimSuffix(base, "."+d)
			ok = true
			break
		}
	}
	if !ok {
		return 0, "", "", fmt.Errorf(
			"postgres: %s must end in .up.sql or .down.sql", filename)
	}

	parts := strings.SplitN(base, "_", 2)
	if len(parts) != 2 {
		return 0, "", "", fmt.Errorf(
			"postgres: %s must be named NNNN_description.%s.sql", filename, direction)
	}
	version, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", "", fmt.Errorf("postgres: %s has a non-numeric version: %w", filename, err)
	}
	return version, parts[1], direction, nil
}

func maxVersion(migrations []migration) int {
	best := 0
	for _, m := range migrations {
		if m.version > best {
			best = m.version
		}
	}
	return best
}
