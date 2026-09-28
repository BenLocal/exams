package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/BenLocal/exams/internal/model"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Stats returns the headline numbers for the list page.
func (s *Store) Stats(ctx context.Context) (model.Stats, error) {
	var st model.Stats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM exams),
			(SELECT count(*) FROM exams WHERE NOT is_read),
			(SELECT count(*) FROM sources),
			(SELECT max(started_at) FROM crawl_runs),
			(SELECT count(*) FROM (
				SELECT DISTINCT ON (source_key) status
				FROM crawl_runs
				ORDER BY source_key, started_at DESC
			) latest WHERE latest.status IN ('failed', 'empty'))`).
		Scan(&st.TotalExams, &st.UnreadExams, &st.SourceCount,
			&st.LastCrawlAt, &st.FailingSources)
	if err != nil {
		return st, fmt.Errorf("stats: %w", err)
	}
	return st, nil
}
