package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/internal/config"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
)

// DB wraps the connection pool and implements lifecycle.Component.
type DB struct {
	pool *pgxpool.Pool
	cfg  config.Database
	log  *slog.Logger
}

// New builds a DB from configuration. It does not connect; Start does.
//
// Separating construction from connection is what lets the supervisor own
// startup ordering and report a failure against a named component, rather than a
// constructor dialling the network as a side effect.
func New(cfg config.Database, log *slog.Logger) *DB {
	return &DB{cfg: cfg, log: log.With(slog.String("component", "postgres"))}
}

// Name identifies the component in lifecycle logs.
func (d *DB) Name() string { return "postgres" }

// Start opens the pool and verifies connectivity.
func (d *DB) Start(ctx context.Context) error {
	poolCfg, err := pgxpool.ParseConfig(d.cfg.URL)
	if err != nil {
		return fmt.Errorf("postgres: parse connection URL: %w", err)
	}

	poolCfg.MaxConns = int32(d.cfg.MaxOpenConns)
	poolCfg.MinConns = int32(d.cfg.MaxIdleConns)
	poolCfg.MaxConnLifetime = d.cfg.ConnMaxLifetime
	poolCfg.MaxConnIdleTime = d.cfg.ConnMaxIdleTime
	poolCfg.ConnConfig.ConnectTimeout = d.cfg.ConnectTimeout

	// Statement cache mode matters on Supabase specifically. The transaction
	// pooler (port 6543) multiplexes connections, so server-side prepared
	// statements are not guaranteed to survive between round trips and produce
	// "prepared statement does not exist" errors under load. Describing by
	// exec avoids relying on them.
	poolCfg.ConnConfig.DefaultQueryExecMode = queryExecModeForURL(d.cfg.URL)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("postgres: create pool: %w", err)
	}

	// Verify connectivity now, with a bounded timeout, so a wrong URL fails at
	// boot rather than on the first request.
	pingCtx, cancel := context.WithTimeout(ctx, d.cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return fmt.Errorf("postgres: ping failed: %w", err)
	}

	d.pool = pool
	d.log.Info("connected",
		slog.Int("max_conns", d.cfg.MaxOpenConns),
		slog.Int("min_conns", d.cfg.MaxIdleConns))

	if d.cfg.AutoMigrate {
		d.log.Warn("auto_migrate is enabled; this is unsafe with more than one instance")
		if err := NewMigrator(pool, d.log).Up(ctx); err != nil {
			pool.Close()
			return err
		}
	}
	return nil
}

// Stop closes the pool, waiting for in-flight queries within the grace period.
func (d *DB) Stop(ctx context.Context) error {
	if d.pool == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		d.pool.Close() // blocks until every connection is returned
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// The supervisor's grace period expired. Report it rather than hanging:
		// a component that routinely overruns is a real bug worth surfacing.
		return ctx.Err()
	}
}

// Pool exposes the underlying pool for the store implementations.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Health reports whether the database is reachable, for /health/ready.
func (d *DB) Health(ctx context.Context) error {
	if d.pool == nil {
		return errors.New("postgres: pool is not initialised")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return d.pool.Ping(ctx)
}

// queryExecModeForURL picks a statement-cache strategy.
//
// Supabase's transaction pooler (port 6543) does not keep a session pinned
// between statements, so a client-side prepared statement may not exist
// server-side on the next round trip — which surfaces as intermittent "prepared
// statement does not exist" errors under load rather than as a clean failure at
// boot. QueryExecModeExec avoids relying on them.
//
// Detecting the pooler by port is crude, but it is what Supabase actually
// documents, and the cost of guessing wrong in the safe direction is only a small
// loss of prepared-statement caching.
func queryExecModeForURL(rawURL string) pgx.QueryExecMode {
	if isTransactionPooler(rawURL) {
		return pgx.QueryExecModeExec
	}
	return pgx.QueryExecModeCacheStatement
}

// isTransactionPooler reports whether the URL points at a transaction-mode pooler.
func isTransactionPooler(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Port() == "6543" {
		return true
	}
	// PgBouncer and Supavisor deployments commonly signal this explicitly.
	q := u.Query()
	return q.Get("pgbouncer") == "true" || q.Get("pool_mode") == "transaction"
}

// isPgErrCode reports whether err is a PostgreSQL error with the given SQLSTATE.
func isPgErrCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// PostgreSQL SQLSTATE codes we branch on.
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
	sqlStateCheckViolation      = "23514"
)

// translateError maps driver errors into domain errors.
//
// Centralised so every store returns the same vocabulary. Without this, one
// store returning a raw pgx error would leak a driver type through the port
// interface and into the application layer — exactly the coupling the layering
// exists to prevent.
func translateError(err error, entity string) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%s: %w", entity, domain.ErrNotFound)
	case isPgErrCode(err, sqlStateUniqueViolation):
		return fmt.Errorf("%s: %w", entity, domain.ErrConflict)
	case isPgErrCode(err, sqlStateForeignKeyViolation):
		// A referenced row is missing. From the caller's perspective this is a
		// validation failure, not a server fault.
		return fmt.Errorf("%s: %w: referenced record does not exist", entity, domain.ErrValidation)
	case isPgErrCode(err, sqlStateCheckViolation):
		return fmt.Errorf("%s: %w: violates a database constraint", entity, domain.ErrValidation)
	default:
		return fmt.Errorf("%s: %w", entity, err)
	}
}
