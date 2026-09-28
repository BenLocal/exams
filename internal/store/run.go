package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/BenLocal/exams/internal/model"
)

// StartRun opens a crawl_runs row in the 'running' state and returns its id.
//
// The row is written before any network work so that a process killed
// mid-crawl still leaves evidence of what it was doing.
func (s *Store) StartRun(ctx context.Context, sourceKey string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO crawl_runs (source_key, status) VALUES ($1, $2) RETURNING id`,
		sourceKey, model.StatusRunning).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("start run: %w", err)
	}
	return id, nil
}

// RunResult is the outcome a finished crawl reports.
type RunResult struct {
	Status    string
	Fetched   int
	Inserted  int
	Updated   int
	Unchanged int
	ErrMsg    string
	Note      string
}

// FinishRun closes a crawl_runs row.
func (s *Store) FinishRun(ctx context.Context, runID int64, r RunResult) error {
	if r.Status == model.StatusRunning {
		return errors.New("FinishRun: status must be a terminal state")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE crawl_runs
		SET status = $2, finished_at = now(), fetched = $3, inserted = $4,
		    updated = $5, unchanged = $6, error = $7, note = $8
		WHERE id = $1`,
		runID, r.Status, r.Fetched, r.Inserted, r.Updated, r.Unchanged, r.ErrMsg, r.Note)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

// ReapStaleRuns closes out rows left in 'running' by a process that died.
// Called at startup, before any new run begins.
func (s *Store) ReapStaleRuns(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE crawl_runs
		SET status = $1, finished_at = now(), error = 'interrupted: process exited mid-run'
		WHERE status = $2`, model.StatusFailed, model.StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("reap stale runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListRuns returns the most recent runs, newest first, optionally filtered to
// one source.
func (s *Store) ListRuns(ctx context.Context, sourceKey string, limit int) ([]model.CrawlRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.source_key, r.status, r.started_at, r.finished_at,
		       r.fetched, r.inserted, r.updated, r.unchanged, r.error, r.note,
		       COALESCE(s.name, r.source_key),
		       (EXTRACT(EPOCH FROM (COALESCE(r.finished_at, now()) - r.started_at)) * 1000)::bigint
		FROM crawl_runs r
		LEFT JOIN sources s ON s.key = r.source_key
		WHERE ($1 = '' OR r.source_key = $1)
		ORDER BY r.started_at DESC
		LIMIT $2`, sourceKey, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []model.CrawlRun
	for rows.Next() {
		var r model.CrawlRun
		if err := rows.Scan(&r.ID, &r.SourceKey, &r.Status, &r.StartedAt, &r.FinishedAt,
			&r.Fetched, &r.Inserted, &r.Updated, &r.Unchanged, &r.Error, &r.Note,
			&r.SourceName, &r.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TryLock takes a process-wide and cross-process advisory lock for a source.
//
// The in-process mutex in the scheduler only guards against overlapping runs
// inside one process; `exams collect` run by hand while the server is up would
// slip past it. A PostgreSQL advisory lock is held on a dedicated pooled
// connection, so it is released automatically if the process dies — which is
// exactly the failure mode a `status='running'` unique index would turn into a
// permanent block.
//
// The returned release func must always be called.
func (s *Store) TryLock(ctx context.Context, key string) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire conn for lock: %w", err)
	}

	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, advisoryKey(key)).Scan(&got); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("advisory lock: %w", err)
	}
	if !got {
		conn.Release()
		return func() {}, false, nil
	}

	return func() {
		// Best effort: if the connection is already broken the lock is gone
		// with it, which is the outcome we want anyway.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, advisoryKey(key))
		conn.Release()
	}, true, nil
}

// advisoryKey derives a stable int64 from a source key.
func advisoryKey(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("exams:" + key))
	return int64(h.Sum64())
}
