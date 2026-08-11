//go:build integration

// Integration tests for the PostgreSQL adapters.
//
// Behind a build tag because they need a real database. Unit tests must stay
// runnable with no infrastructure, or people stop running them; these verify the
// things a fake cannot: actual SQL, real constraints, and the concurrency
// guarantees the schema is relied upon to provide.
//
//	docker compose -f docker/docker-compose.yml up -d postgres
//	BEUVIAN_TEST_DB_URL='postgres://beuvian:beuvian_local_dev@127.0.0.1:5432/beuvian?sslmode=disable' \
//	  go test -tags=integration ./internal/adapter/postgres/
package postgres_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bhuvan0808/beuviancode/backend/internal/adapter/postgres"
	"github.com/bhuvan0808/beuviancode/backend/internal/domain"
	"github.com/bhuvan0808/beuviancode/backend/internal/port"
	"github.com/bhuvan0808/beuviancode/shared/id"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("BEUVIAN_TEST_DB_URL")
	if url == "" {
		t.Skip("BEUVIAN_TEST_DB_URL is not set; skipping integration tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// seedUser creates a user and returns it. Each test gets its own so tests never
// contend over shared rows.
func seedUser(t *testing.T, pool *pgxpool.Pool) domain.User {
	t.Helper()
	users := postgres.NewUserStore(pool)

	// A random GitHub ID keeps repeated runs from colliding on the unique index.
	ghID := int64(time.Now().UnixNano()%1_000_000_000) + int64(len(t.Name()))
	u, err := users.UpsertByGitHub(context.Background(),
		domain.User{
			ID: id.WithPrefix(id.PrefixUser), GitHubID: ghID,
			GitHubLogin: "tester", Name: "Test User", LastLoginAt: time.Now().UTC(),
		},
		domain.OAuthAccount{
			ID: id.WithPrefix("oap"), Provider: domain.ProviderGitHub,
			ProviderUserID: id.New(), Scopes: []string{"read:user"},
		})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}

func seedDevice(t *testing.T, pool *pgxpool.Pool, userID string, caps ...string) domain.Device {
	t.Helper()
	devices := postgres.NewDeviceStore(pool)
	d := domain.Device{
		ID: id.WithPrefix(id.PrefixDevice), UserID: userID,
		Name: "test-device", Platform: "linux/amd64",
		Capabilities:   caps,
		TokenHash:      id.New(),
		TokenExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if d.Capabilities == nil {
		d.Capabilities = []string{}
	}
	if err := devices.Create(context.Background(), d); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	return d
}

func TestUpsertByGitHubIsIdempotentAndKeyedOnID(t *testing.T) {
	pool := testPool(t)
	users := postgres.NewUserStore(pool)
	ctx := context.Background()

	ghID := time.Now().UnixNano() % 1_000_000_000

	first, err := users.UpsertByGitHub(ctx,
		domain.User{ID: id.WithPrefix(id.PrefixUser), GitHubID: ghID, GitHubLogin: "octocat"},
		domain.OAuthAccount{ID: id.WithPrefix("oap"), Provider: domain.ProviderGitHub, ProviderUserID: "1"})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// The same GitHub account logging in again after a RENAME must resolve to the
	// same Beuvian user. Keying on the login instead would create a second account
	// and strand their devices and history.
	second, err := users.UpsertByGitHub(ctx,
		domain.User{ID: id.WithPrefix(id.PrefixUser), GitHubID: ghID, GitHubLogin: "octocat-renamed"},
		domain.OAuthAccount{ID: id.WithPrefix("oap"), Provider: domain.ProviderGitHub, ProviderUserID: "1"})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("a renamed GitHub account produced a new user: %s -> %s", first.ID, second.ID)
	}
	if second.GitHubLogin != "octocat-renamed" {
		t.Errorf("login was not updated: %q", second.GitHubLogin)
	}

	// Settings must exist from first login, so no read path has to handle absence.
	if _, err := users.Settings(ctx, first.ID); err != nil {
		t.Errorf("settings row was not created on first login: %v", err)
	}
}

func TestDeviceOwnershipIsEnforcedInTheQuery(t *testing.T) {
	pool := testPool(t)
	devices := postgres.NewDeviceStore(pool)
	ctx := context.Background()

	owner := seedUser(t, pool)
	device := seedDevice(t, pool, owner.ID)
	stranger := seedUser(t, pool)

	if _, err := devices.ByIDForUser(ctx, device.ID, owner.ID); err != nil {
		t.Fatalf("owner should be able to read their device: %v", err)
	}

	// Another user's device must be ErrNotFound, not ErrForbidden. A 403 would
	// confirm the ID exists and enable enumeration.
	_, err := devices.ByIDForUser(ctx, device.ID, stranger.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for another user's device", err)
	}
}

func TestOnlyOneActiveSessionPerDevice(t *testing.T) {
	pool := testPool(t)
	sessions := postgres.NewSessionStore(pool)
	ctx := context.Background()

	user := seedUser(t, pool)
	device := seedDevice(t, pool, user.ID, "claude")

	mk := func() domain.Session {
		return domain.Session{
			ID: id.WithPrefix(id.PrefixSession), UserID: user.ID, DeviceID: device.ID,
			Adapter: "claude", State: protocol.StateStarting,
			WorkingDirectory: "/src", StartedAt: time.Now().UTC(),
		}
	}

	if err := sessions.Create(ctx, mk()); err != nil {
		t.Fatalf("first session: %v", err)
	}

	// The unique partial index is the real guarantee. An application-level check
	// alone would let two concurrent requests both pass and both insert.
	err := sessions.Create(ctx, mk())
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("err = %v, want ErrConflict for a second live session", err)
	}
}

func TestConcurrentSessionStartsProduceExactlyOneWinner(t *testing.T) {
	// The property the index exists for: under real concurrency, exactly one
	// session survives. This is what an application-level read-then-write cannot
	// provide, and it is why the constraint lives in the schema.
	pool := testPool(t)
	sessions := postgres.NewSessionStore(pool)
	ctx := context.Background()

	user := seedUser(t, pool)
	device := seedDevice(t, pool, user.ID, "claude")

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = sessions.Create(ctx, domain.Session{
				ID: id.WithPrefix(id.PrefixSession), UserID: user.ID, DeviceID: device.ID,
				Adapter: "claude", State: protocol.StateStarting,
				WorkingDirectory: "/src", StartedAt: time.Now().UTC(),
			})
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d concurrent starts succeeded, want exactly 1", succeeded)
	}
}

func TestLogIngestionIsIdempotent(t *testing.T) {
	pool := testPool(t)
	sessions := postgres.NewSessionStore(pool)
	logs := postgres.NewSessionLogStore(pool)
	ctx := context.Background()

	user := seedUser(t, pool)
	device := seedDevice(t, pool, user.ID)
	sess := domain.Session{
		ID: id.WithPrefix(id.PrefixSession), UserID: user.ID, DeviceID: device.ID,
		Adapter: "claude", State: protocol.StateRunning,
		WorkingDirectory: "/src", StartedAt: time.Now().UTC(),
	}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	batch := []domain.SessionLog{
		{SessionID: sess.ID, Seq: 1, Stream: protocol.StreamStdout, Content: "line one", At: time.Now().UTC()},
		{SessionID: sess.ID, Seq: 2, Stream: protocol.StreamStdout, Content: "line two", At: time.Now().UTC()},
	}

	if err := logs.AppendBatch(ctx, batch); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// A redelivered batch after a reconnect must not duplicate the transcript.
	if err := logs.AppendBatch(ctx, batch); err != nil {
		t.Fatalf("redelivered append should not error: %v", err)
	}

	stored, err := logs.After(ctx, sess.ID, 0, 100)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if len(stored) != 2 {
		t.Errorf("got %d rows after redelivery, want 2 (no duplicates)", len(stored))
	}

	maxSeq, err := logs.MaxSeq(ctx, sess.ID)
	if err != nil {
		t.Fatalf("max seq: %v", err)
	}
	if maxSeq != 2 {
		t.Errorf("MaxSeq = %d, want 2", maxSeq)
	}
}

func TestPromptSurvivesAsPendingAndIsFoundForRedelivery(t *testing.T) {
	// The durability guarantee behind ADR-0006: once enqueued, a prompt is found
	// again even if every cache and in-memory structure is lost.
	pool := testPool(t)
	queue := postgres.NewPromptQueueStore(pool)
	ctx := context.Background()

	user := seedUser(t, pool)
	device := seedDevice(t, pool, user.ID, "claude")

	prompt := domain.QueuedPrompt{
		ID: id.WithPrefix(id.PrefixPrompt), UserID: user.ID, DeviceID: device.ID,
		Text: "Now implement authentication.", Status: domain.PromptPending,
		EnqueuedAt: time.Now().UTC(), CorrelationID: id.WithPrefix(id.PrefixCorrelation),
	}
	if err := queue.Enqueue(ctx, prompt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	pending, err := queue.PendingForDevice(ctx, device.ID, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != prompt.ID {
		t.Fatalf("pending = %+v, want the enqueued prompt", pending)
	}

	// A dispatched-but-unacknowledged prompt must STILL be redeliverable: the
	// device may have disconnected before acknowledging.
	pending[0].MarkDispatched(time.Now().UTC())
	if err := queue.Update(ctx, pending[0]); err != nil {
		t.Fatalf("update: %v", err)
	}
	stillPending, err := queue.PendingForDevice(ctx, device.ID, 10)
	if err != nil {
		t.Fatalf("pending after dispatch: %v", err)
	}
	if len(stillPending) != 1 {
		t.Errorf("a dispatched prompt must remain redeliverable, got %d", len(stillPending))
	}

	// Only acknowledgement removes it from the queue.
	stillPending[0].MarkDelivered(time.Now().UTC())
	if err := queue.Update(ctx, stillPending[0]); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	after, err := queue.PendingForDevice(ctx, device.ID, 10)
	if err != nil {
		t.Fatalf("pending after delivery: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("a delivered prompt should leave the queue, got %d", len(after))
	}
}

func TestRefreshTokenReuseIsDetected(t *testing.T) {
	pool := testPool(t)
	tokens := postgres.NewRefreshTokenStore(pool)
	ctx := context.Background()

	user := seedUser(t, pool)
	family := id.WithPrefix("fam")

	tok := domain.RefreshToken{
		ID: id.WithPrefix("rft"), UserID: user.ID, TokenHash: id.New(),
		FamilyID: family, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := tokens.Create(ctx, tok); err != nil {
		t.Fatalf("create: %v", err)
	}

	// First rotation succeeds.
	if err := tokens.MarkUsed(ctx, tok.ID, time.Now().UTC()); err != nil {
		t.Fatalf("first MarkUsed: %v", err)
	}

	// A second presentation means two parties hold the token. The store must
	// report it so the caller can revoke the whole family.
	err := tokens.MarkUsed(ctx, tok.ID, time.Now().UTC())
	if !errors.Is(err, domain.ErrTokenReused) {
		t.Errorf("err = %v, want ErrTokenReused on second use", err)
	}

	// Revoking the family removes it entirely: keeping a revoked credential row
	// is the opposite of what revocation means.
	if err := tokens.RevokeFamily(ctx, family); err != nil {
		t.Fatalf("revoke family: %v", err)
	}
	if _, err := tokens.ByHash(ctx, tok.TokenHash); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want the token to be gone after family revocation", err)
	}
}

func TestStatusUpsertIgnoresOutOfOrderFrames(t *testing.T) {
	// Under reconnect and redelivery an older STATUS can arrive after a newer one.
	// Without the reported_at guard the dashboard would flicker backwards.
	pool := testPool(t)
	devices := postgres.NewDeviceStore(pool)
	ctx := context.Background()

	user := seedUser(t, pool)
	device := seedDevice(t, pool, user.ID)

	newer := time.Now().UTC()
	older := newer.Add(-time.Minute)

	if err := devices.SaveStatus(ctx, domain.AgentStatus{
		DeviceID: device.ID, State: "running", CPUPercent: 50, ReportedAt: newer,
	}); err != nil {
		t.Fatalf("save newer: %v", err)
	}
	if err := devices.SaveStatus(ctx, domain.AgentStatus{
		DeviceID: device.ID, State: "idle", CPUPercent: 1, ReportedAt: older,
	}); err != nil {
		t.Fatalf("save older: %v", err)
	}

	got, err := devices.Status(ctx, device.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got.State != "running" {
		t.Errorf("State = %q, want the newer frame to win", got.State)
	}
}

func TestStaleSessionsAreClosed(t *testing.T) {
	// A crashed agent leaves a session that looks live forever, and the unique
	// index then blocks the user from ever starting another on that device. The
	// sweep is what makes that recoverable.
	pool := testPool(t)
	sessions := postgres.NewSessionStore(pool)
	devices := postgres.NewDeviceStore(pool)
	ctx := context.Background()

	user := seedUser(t, pool)
	device := seedDevice(t, pool, user.ID)

	// Last seen well in the past.
	if err := devices.TouchLastSeen(ctx, device.ID, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	sess := domain.Session{
		ID: id.WithPrefix(id.PrefixSession), UserID: user.ID, DeviceID: device.ID,
		Adapter: "claude", State: protocol.StateRunning,
		WorkingDirectory: "/src", StartedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := sessions.EndStaleSessions(ctx, time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n < 1 {
		t.Fatalf("swept %d sessions, want at least 1", n)
	}

	closed, err := sessions.ByIDForUser(ctx, sess.ID, user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if closed.Active() {
		t.Error("the stale session should be closed")
	}

	// And a new session must now be startable on that device.
	if err := sessions.Create(ctx, domain.Session{
		ID: id.WithPrefix(id.PrefixSession), UserID: user.ID, DeviceID: device.ID,
		Adapter: "claude", State: protocol.StateStarting,
		WorkingDirectory: "/src", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Errorf("a new session should be possible after the sweep: %v", err)
	}
}

func TestCursorPaginationDoesNotSkipOrRepeat(t *testing.T) {
	pool := testPool(t)
	sessions := postgres.NewSessionStore(pool)
	ctx := context.Background()

	user := seedUser(t, pool)

	// Each session needs its own device: only one may be live per device.
	const total = 7
	for i := 0; i < total; i++ {
		device := seedDevice(t, pool, user.ID)
		sess := domain.Session{
			ID: id.WithPrefix(id.PrefixSession), UserID: user.ID, DeviceID: device.ID,
			Adapter: "claude", State: protocol.StateStopped,
			WorkingDirectory: "/src", StartedAt: time.Now().UTC(),
		}
		ended := time.Now().UTC()
		sess.EndedAt = &ended
		if err := sessions.Create(ctx, sess); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		batch, next, err := sessions.List(ctx,
			port.SessionFilter{UserID: user.ID},
			port.Page{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, s := range batch {
			if seen[s.ID] {
				t.Errorf("session %s appeared on two pages", s.ID)
			}
			seen[s.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != total {
		t.Errorf("paged through %d sessions, want %d", len(seen), total)
	}
}

func TestAuditWriteNeverFailsTheCaller(t *testing.T) {
	// An audit write that could fail a request would make auditing a liability.
	pool := testPool(t)
	audit := postgres.NewAuditStore(pool, blog.Discard())
	user := seedUser(t, pool)

	// A deliberately invalid entry (unknown user) must not panic or block.
	audit.Record(context.Background(), domain.AuditEntry{
		UserID: "usr_does_not_exist", Action: "test.action",
	})
	audit.Record(context.Background(), domain.AuditEntry{
		UserID: user.ID, Action: domain.ActionUserLogin,
		TargetType: "user", TargetID: user.ID,
		Metadata: map[string]any{"ok": true},
	})
}
