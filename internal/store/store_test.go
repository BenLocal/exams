package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenLocal/exams/internal/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The store tests run against a real PostgreSQL, because the behaviour worth
// testing here — ON CONFLICT semantics, partial index predicates, advisory
// locks, transaction rollback — is exactly the behaviour a fake would have to
// reimplement, and would then be testing itself rather than the database.
//
// They are destructive: every test truncates the tables. Pointing them at a
// database with real data would delete it, so the harness refuses unless the
// database name looks like a test one.
const (
	dsnEnv     = "TEST_DATABASE_URL"
	allowAnyDB = "TEST_DATABASE_URL_ALLOW_ANY"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

// testStore returns a Store against a truncated, migrated test database.
//
// The test is skipped when TEST_DATABASE_URL is unset, so `go test ./...`
// stays green on a machine with no database.
func testStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping store integration tests "+
			"(see the Makefile's test-store target)", dsnEnv)
	}
	guardDestructive(t, dsn)

	poolOnce.Do(func() { pool, poolErr = openTestPool(ctx, dsn) })
	if poolErr != nil {
		t.Fatalf("connect to %s: %v", dsnEnv, poolErr)
	}

	st := &Store{pool: pool}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	truncateAll(t, st)
	return st
}

// guardDestructive refuses to run against a database whose name does not look
// like a test one.
func guardDestructive(t *testing.T, dsn string) {
	t.Helper()
	if os.Getenv(allowAnyDB) == "1" {
		return
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", dsnEnv, err)
	}
	name := cfg.ConnConfig.Database
	if !strings.Contains(strings.ToLower(name), "test") {
		t.Fatalf(
			"refusing to run: %s points at database %q, which does not look like a test database.\n"+
				"These tests TRUNCATE every table. Use a database whose name contains \"test\",\n"+
				"or set %s=1 if you really mean it.",
			dsnEnv, name, allowAnyDB)
	}
}

func openTestPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// truncateAll empties every table and resets sequences, so ids are
// predictable across tests.
func truncateAll(t *testing.T, st *Store) {
	t.Helper()
	_, err := st.pool.Exec(context.Background(),
		`TRUNCATE exam_changes, notifications, exams, crawl_runs, sources RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// seedSource inserts a source row, which exams reference by key.
func seedSource(t *testing.T, st *Store, key string) {
	t.Helper()
	if err := st.SyncSources(context.Background(), []model.Source{
		{Key: key, Name: "测试源 " + key, BaseURL: "https://example.test"},
	}); err != nil {
		t.Fatalf("seed source %s: %v", key, err)
	}
}

// item builds a minimal valid Item.
func item(externalID, title string) model.Item {
	return model.Item{
		ExternalID: externalID,
		Title:      title,
		URL:        "https://example.test/" + externalID,
		Summary:    "摘要 " + title,
		Content:    "正文 " + title,
		Category:   "测试分类",
		Region:     "全国",
	}
}

// at returns a pointer to a time offset from a fixed base.
func at(daysAgo int) *time.Time {
	t := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC).AddDate(0, 0, -daysAgo)
	return &t
}

// startRun opens a crawl_runs row and returns its id.
//
// exam_changes.run_id is a foreign key, so an audit row cannot be written
// without a real run; passing a placeholder id would fail the constraint
// rather than test anything.
func startRun(t *testing.T, st *Store, sourceKey string) int64 {
	t.Helper()
	id, err := st.StartRun(context.Background(), sourceKey)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return id
}

// upsert is a helper that fails the test on error.
func upsert(t *testing.T, st *Store, sourceKey string, items ...model.Item) model.UpsertOutcome {
	t.Helper()
	out, err := st.UpsertItems(context.Background(), sourceKey, startRun(t, st, sourceKey), items)
	if err != nil {
		t.Fatalf("UpsertItems: %v", err)
	}
	return out
}

func countExams(t *testing.T, st *Store, sourceKey string) int {
	t.Helper()
	var n int
	err := st.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM exams WHERE source_key = $1`, sourceKey).Scan(&n)
	if err != nil {
		t.Fatalf("count exams: %v", err)
	}
	return n
}
