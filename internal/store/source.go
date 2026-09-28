package store

import (
	"context"
	"fmt"
	"time"

	"github.com/BenLocal/exams/internal/model"
	"github.com/jackc/pgx/v5"
)

// SyncSources upserts the collector registry into the sources table.
//
// `enabled` and `interval` are deliberately left untouched on conflict: those
// are operator decisions made through the web UI, and a restart must not
// silently undo them. Only descriptive fields are refreshed.
func (s *Store) SyncSources(ctx context.Context, srcs []model.Source) error {
	if len(srcs) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		for _, src := range srcs {
			_, err := tx.Exec(ctx, `
				INSERT INTO sources (key, name, base_url)
				VALUES ($1, $2, $3)
				ON CONFLICT (key) DO UPDATE SET
					name       = EXCLUDED.name,
					base_url   = EXCLUDED.base_url,
					updated_at = now()`,
				src.Key, src.Name, src.BaseURL)
			if err != nil {
				return fmt.Errorf("sync source %s: %w", src.Key, err)
			}
		}
		return nil
	})
}

const sourceColumns = `key, name, base_url, enabled, interval, bootstrap_done,
	last_run_at, created_at, updated_at`

func scanSource(row interface{ Scan(...any) error }) (model.Source, error) {
	var s model.Source
	err := row.Scan(&s.Key, &s.Name, &s.BaseURL, &s.Enabled, &s.Interval,
		&s.BootstrapDone, &s.LastRunAt, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// ListSources returns every configured source, ordered by name.
func (s *Store) ListSources(ctx context.Context) ([]model.Source, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sourceColumns+` FROM sources ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var out []model.Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// GetSource returns one source by key.
func (s *Store) GetSource(ctx context.Context, key string) (model.Source, error) {
	src, err := scanSource(s.pool.QueryRow(ctx, `SELECT `+sourceColumns+` FROM sources WHERE key = $1`, key))
	if isNoRows(err) {
		return model.Source{}, fmt.Errorf("source %q not found", key)
	}
	return src, err
}

// SetSourceEnabled turns crawling for a source on or off.
func (s *Store) SetSourceEnabled(ctx context.Context, key string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sources SET enabled = $2, updated_at = now() WHERE key = $1`, key, enabled)
	if err != nil {
		return fmt.Errorf("set source enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("source %q not found", key)
	}
	return nil
}

// SetSourceInterval overrides the global cron spec for one source. An empty
// spec restores the global default.
func (s *Store) SetSourceInterval(ctx context.Context, key, interval string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sources SET interval = $2, updated_at = now() WHERE key = $1`, key, interval)
	if err != nil {
		return fmt.Errorf("set source interval: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("source %q not found", key)
	}
	return nil
}

// MarkSourceRun records that a run finished for a source, and marks the source
// as bootstrapped so later runs are allowed to notify.
func (s *Store) MarkSourceRun(ctx context.Context, key string, at time.Time, bootstrapDone bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sources
		SET last_run_at = $2, bootstrap_done = bootstrap_done OR $3, updated_at = now()
		WHERE key = $1`, key, at, bootstrapDone)
	if err != nil {
		return fmt.Errorf("mark source run: %w", err)
	}
	return nil
}
