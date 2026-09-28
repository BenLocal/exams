package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/BenLocal/exams/migrations"
	"github.com/jackc/pgx/v5"
)

// Migration is one SQL file from the embedded migrations directory.
type Migration struct {
	Version  string // filename, e.g. "0001_init.sql"
	SQL      string
	Optional bool // ".optional.sql" — a failure warns instead of aborting
}

// MigrationResult reports the outcome of one migration.
type MigrationResult struct {
	Version  string
	Optional bool
	Applied  bool   // this run executed it (as opposed to it already being in place)
	Skipped  bool   // already applied
	Err      error  // non-nil only for a failed optional migration
	Note     string // human-readable outcome for the CLI
}

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now(),
    note       text NOT NULL DEFAULT ''
)`

// loadMigrations reads the embedded files in filename order.
func loadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var out []Migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		out = append(out, Migration{
			Version:  name,
			SQL:      string(body),
			Optional: strings.HasSuffix(name, ".optional.sql"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies every pending migration.
//
// Required migrations run inside a transaction together with their
// schema_migrations row, so a failure is atomic. Optional migrations that fail
// are recorded with a note and retried on the next invocation — that way
// granting a missing privilege later is enough to pick them up, with no manual
// bookkeeping.
func (s *Store) Migrate(ctx context.Context) ([]MigrationResult, error) {
	if _, err := s.pool.Exec(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	migs, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	type state struct {
		note string
	}
	applied := map[string]state{}
	rows, err := s.pool.Query(ctx, `SELECT version, note FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v, n string
		if err := rows.Scan(&v, &n); err != nil {
			rows.Close()
			return nil, err
		}
		applied[v] = state{note: n}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var results []MigrationResult
	for _, m := range migs {
		prev, seen := applied[m.Version]
		// Retry an optional migration only if it failed last time.
		if seen && prev.note == "" {
			results = append(results, MigrationResult{
				Version: m.Version, Optional: m.Optional, Skipped: true,
				Note: "already applied",
			})
			continue
		}

		if err := s.applyMigration(ctx, m); err != nil {
			if !m.Optional {
				return results, fmt.Errorf("migration %s: %w", m.Version, err)
			}
			// Record the failure so we do not warn on every serve, but keep it
			// retryable by leaving a non-empty note.
			note := truncate(err.Error(), 500)
			if _, recErr := s.pool.Exec(ctx,
				`INSERT INTO schema_migrations (version, note) VALUES ($1, $2)
				 ON CONFLICT (version) DO UPDATE SET note = EXCLUDED.note, applied_at = now()`,
				m.Version, note); recErr != nil {
				return results, fmt.Errorf("record skipped migration %s: %w", m.Version, recErr)
			}
			results = append(results, MigrationResult{
				Version: m.Version, Optional: true, Err: err,
				Note: "skipped (see warning)",
			})
			continue
		}

		results = append(results, MigrationResult{
			Version: m.Version, Applied: true, Optional: m.Optional, Note: "applied",
		})
	}
	return results, nil
}

// applyMigration executes one file and records it in the same transaction, so
// a migration can never be marked applied without its DDL having committed.
func (s *Store) applyMigration(ctx context.Context, m Migration) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, note) VALUES ($1, '')
			 ON CONFLICT (version) DO UPDATE SET note = '', applied_at = now()`,
			m.Version)
		return err
	})
}

// PendingRequired reports required migrations that have not been applied.
//
// `exams serve` calls this so a missing schema produces one clear message
// instead of a stream of "relation does not exist" errors.
func (s *Store) PendingRequired(ctx context.Context) ([]string, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if !exists {
		migs, err := loadMigrations()
		if err != nil {
			return nil, err
		}
		var names []string
		for _, m := range migs {
			if !m.Optional {
				names = append(names, m.Version)
			}
		}
		return names, nil
	}

	migs, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	rows, err := s.pool.Query(ctx, `SELECT version FROM schema_migrations WHERE note = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var pending []string
	for _, m := range migs {
		if !m.Optional && !applied[m.Version] {
			pending = append(pending, m.Version)
		}
	}
	return pending, nil
}

// truncate shortens s to at most n bytes for storage in a note column.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
