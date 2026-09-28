package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BenLocal/exams/internal/model"
	"github.com/jackc/pgx/v5"
)

// examListColumns deliberately omits `content`: list queries must not drag
// every body through the wire, and the list template only ever renders the
// summary.
const examListColumns = `e.id, e.source_key, e.external_id, e.title, e.url, e.summary,
	e.category, e.region, e.published_at, e.published_raw, e.deadline_at,
	e.list_hash, e.body_hash, e.is_read, e.first_seen_at, e.last_seen_at, e.updated_at,
	COALESCE(s.name, e.source_key)`

// scanListRow reads one row of examListColumns.
func scanListRow(rows pgx.Rows) (model.Exam, error) {
	var e model.Exam
	err := rows.Scan(&e.ID, &e.SourceKey, &e.ExternalID, &e.Title, &e.URL, &e.Summary,
		&e.Category, &e.Region, &e.PublishedAt, &e.PublishedRaw, &e.DeadlineAt,
		&e.ListHash, &e.BodyHash, &e.IsRead, &e.FirstSeenAt, &e.LastSeenAt,
		&e.UpdatedAt, &e.SourceName)
	return e, err
}

// existingExam is the stored state needed to classify an incoming item.
type existingExam struct {
	id           int64
	title        string
	url          string
	summary      string
	category     string
	region       string
	publishedAt  *time.Time
	publishedRaw string
	deadlineAt   *time.Time
	bodyHash     string
}

// StoredHash is the persisted fingerprint of an exam.
type StoredHash struct {
	ListHash string
	// BodyHash is empty when the detail page has never been fetched
	// successfully. That is what makes a body fetch retryable.
	BodyHash string
}

// StoredHashes returns external_id -> hashes for one source.
//
// The crawl runner uses this to decide which items need their detail page
// fetched: only items that are new, whose list-level fields moved, or whose
// body was never obtained. That last case matters — without it, an item whose
// detail fetch failed or was cut short by a timeout would be stored with an
// empty body and then skipped forever, because its list hash now matches.
func (s *Store) StoredHashes(ctx context.Context, sourceKey string) (map[string]StoredHash, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT external_id, list_hash, body_hash FROM exams WHERE source_key = $1`, sourceKey)
	if err != nil {
		return nil, fmt.Errorf("stored hashes: %w", err)
	}
	defer rows.Close()

	out := map[string]StoredHash{}
	for rows.Next() {
		var id string
		var h StoredHash
		if err := rows.Scan(&id, &h.ListHash, &h.BodyHash); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}

// UpsertItems writes a batch of scraped items in a single transaction.
//
// The whole batch commits or none of it does, so a failure part-way through a
// source never leaves a half-imported crawl behind. `is_read` and
// `first_seen_at` are never touched on update: a re-crawl must not resurrect a
// notice the user has already read.
func (s *Store) UpsertItems(ctx context.Context, sourceKey string, runID int64, items []model.Item) (model.UpsertOutcome, error) {
	var out model.UpsertOutcome
	if len(items) == 0 {
		return out, nil
	}

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		out = model.UpsertOutcome{}
		for _, it := range items {
			res, err := upsertOne(ctx, tx, sourceKey, runID, it)
			if err != nil {
				return fmt.Errorf("upsert %s/%s: %w", sourceKey, it.ExternalID, err)
			}
			exam := model.Exam{
				ID:          res.ExamID,
				SourceKey:   sourceKey,
				Title:       it.Title,
				URL:         it.URL,
				Summary:     it.Summary,
				Category:    it.Category,
				Region:      it.Region,
				PublishedAt: it.PublishedAt,
				DeadlineAt:  it.DeadlineAt,
			}
			switch res.Action {
			case model.ActionCreated:
				out.Inserted++
				out.Created = append(out.Created, exam)
			case model.ActionUpdated:
				out.Updated++
				out.Changed = append(out.Changed, exam)
			default:
				out.Unchanged++
			}
		}
		return nil
	})
	return out, err
}

// upsertOne creates or refreshes a single exam and records an audit row when
// something actually changed.
func upsertOne(ctx context.Context, tx pgx.Tx, sourceKey string, runID int64, it model.Item) (model.UpsertResult, error) {
	listHash := it.ListHash()

	// An empty body hashes to a perfectly valid digest, which would make
	// "body_hash <> ''" useless as a "do we have the body yet?" test — and the
	// crawl runner relies on exactly that test to decide whether to retry a
	// detail fetch. So an absent body is recorded as an empty hash.
	bodyHash := ""
	if strings.TrimSpace(it.Content) != "" {
		bodyHash = it.BodyHash()
	}

	var prev existingExam
	found := true
	err := tx.QueryRow(ctx, `
		SELECT id, title, url, summary, category, region,
		       published_at, published_raw, deadline_at, body_hash
		FROM exams WHERE source_key = $1 AND external_id = $2`,
		sourceKey, it.ExternalID).
		Scan(&prev.id, &prev.title, &prev.url, &prev.summary, &prev.category, &prev.region,
			&prev.publishedAt, &prev.publishedRaw, &prev.deadlineAt, &prev.bodyHash)
	switch {
	case err == nil:
	case isNoRows(err):
		found = false
	default:
		return model.UpsertResult{}, err
	}

	// A crawl only fetches a detail page when the list-level fields moved, so
	// most items arrive with no body. An absent body must never overwrite a
	// stored one.
	contentProvided := strings.TrimSpace(it.Content) != ""

	action := model.ActionCreated
	changed := []string{}
	if found {
		changed = diffExam(prev, it, bodyHash, contentProvided)
		if len(changed) == 0 {
			action = model.ActionUnchanged
		} else {
			action = model.ActionUpdated
		}
	}

	var examID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO exams (source_key, external_id, title, url, summary, content,
		                   category, region, published_at, published_raw, deadline_at,
		                   list_hash, body_hash, first_seen_at, last_seen_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now(), now(), now())
		ON CONFLICT (source_key, external_id) DO UPDATE SET
			title         = EXCLUDED.title,
			url           = EXCLUDED.url,
			summary       = EXCLUDED.summary,
			-- Keep the stored body when this run did not fetch one.
			content       = CASE WHEN EXCLUDED.content = '' THEN exams.content
			                     ELSE EXCLUDED.content END,
			body_hash     = CASE WHEN EXCLUDED.content = '' THEN exams.body_hash
			                     ELSE EXCLUDED.body_hash END,
			category      = EXCLUDED.category,
			region        = EXCLUDED.region,
			published_at  = EXCLUDED.published_at,
			published_raw = EXCLUDED.published_raw,
			deadline_at   = EXCLUDED.deadline_at,
			list_hash     = EXCLUDED.list_hash,
			last_seen_at  = now(),
			updated_at    = CASE
				WHEN exams.list_hash IS DISTINCT FROM EXCLUDED.list_hash
				  OR (EXCLUDED.content <> '' AND exams.body_hash IS DISTINCT FROM EXCLUDED.body_hash)
				THEN now() ELSE exams.updated_at END
		RETURNING id`,
		sourceKey, it.ExternalID, it.Title, it.URL, it.Summary, it.Content,
		it.Category, it.Region, it.PublishedAt, it.PublishedRaw, it.DeadlineAt,
		listHash, bodyHash).Scan(&examID)
	if err != nil {
		return model.UpsertResult{}, err
	}

	if action != model.ActionUnchanged {
		if _, err := tx.Exec(ctx, `
			INSERT INTO exam_changes (exam_id, run_id, change_type, changed_fields)
			VALUES ($1, $2, $3, $4)`, examID, runID, action, changed); err != nil {
			return model.UpsertResult{}, err
		}
	}

	return model.UpsertResult{
		Action:        action,
		ExamID:        examID,
		ChangedFields: changed,
	}, nil
}

// diffExam lists the fields that moved. Only body *hashes* are compared, so a
// content change is detected without loading either body.
//
// contentProvided is false when this run never fetched the detail page; the
// body is then unknown rather than empty, and must not be reported as changed.
func diffExam(prev existingExam, it model.Item, newBodyHash string, contentProvided bool) []string {
	changed := []string{}
	add := func(field string, differs bool) {
		if differs {
			changed = append(changed, field)
		}
	}
	add("title", prev.title != it.Title)
	add("url", prev.url != it.URL)
	add("summary", prev.summary != it.Summary)
	add("category", prev.category != it.Category)
	add("region", prev.region != it.Region)
	add("published_at", !timeEqual(prev.publishedAt, it.PublishedAt))
	add("published_raw", prev.publishedRaw != it.PublishedRaw)
	add("deadline_at", !timeEqual(prev.deadlineAt, it.DeadlineAt))
	add("content", contentProvided && prev.bodyHash != newBodyHash)
	return changed
}

func timeEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// ---------------------------------------------------------------------------
// Filtering

type whereBuilder struct {
	conds []string
	args  []any
}

func (b *whereBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *whereBuilder) sql() string {
	if len(b.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(b.conds, " AND ")
}

// likeEscaper neutralises ILIKE metacharacters so a search for "50%" searches
// for a literal percent sign instead of matching everything.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func newExamWhere(f model.ItemFilter) *whereBuilder {
	b := &whereBuilder{}

	if q := strings.TrimSpace(f.Query); q != "" {
		// The pattern is a single parameter rather than a '%'||$1||'%'
		// expression so the trigram index can still be used.
		p := b.arg("%" + likeEscaper.Replace(q) + "%")
		// category and region are matched exactly rather than as substrings.
		// People search for "考研" or "四六级", which are category values, not
		// words that appear in the titles ("全国硕士研究生招生考试" carries no
		// such string). It also keeps those two predicates from forcing the
		// whole OR onto a sequential scan.
		exact := b.arg(q)
		b.conds = append(b.conds, "(e.title ILIKE "+p+` ESCAPE '\'`+
			" OR e.summary ILIKE "+p+` ESCAPE '\'`+
			" OR e.content ILIKE "+p+` ESCAPE '\'`+
			" OR e.category = "+exact+
			" OR e.region = "+exact+")")
	}
	if f.Source != "" {
		b.conds = append(b.conds, "e.source_key = "+b.arg(f.Source))
	}
	if f.Category != "" {
		b.conds = append(b.conds, "e.category = "+b.arg(f.Category))
	}
	if f.Region != "" {
		b.conds = append(b.conds, "e.region = "+b.arg(f.Region))
	}
	if f.Unread {
		b.conds = append(b.conds, "NOT e.is_read")
	}
	return b
}

// ListExams returns one page of results plus the metadata the template needs.
func (s *Store) ListExams(ctx context.Context, f model.ItemFilter) (model.Page, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PerPage < 1 || f.PerPage > 200 {
		f.PerPage = 20
	}

	w := newExamWhere(f)

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM exams e`+w.sql(), w.args...).Scan(&total); err != nil {
		return model.Page{}, fmt.Errorf("count exams: %w", err)
	}

	totalPages := (total + f.PerPage - 1) / f.PerPage
	// An out-of-range page — a stale bookmark, or a filter that shrank the
	// result set — is clamped to the last page. Returning an empty list would
	// read as "nothing matched" when the filter is actually fine.
	if totalPages > 0 && f.Page > totalPages {
		f.Page = totalPages
	}

	n := len(w.args)
	args := append(append([]any{}, w.args...), f.PerPage, f.Offset())
	limitPh := "$" + strconv.Itoa(n+1)
	offsetPh := "$" + strconv.Itoa(n+2)

	rows, err := s.pool.Query(ctx, `SELECT `+examListColumns+`
		FROM exams e
		LEFT JOIN sources s ON s.key = e.source_key`+w.sql()+`
		ORDER BY e.published_at DESC NULLS LAST, e.id DESC
		LIMIT `+limitPh+` OFFSET `+offsetPh, args...)
	if err != nil {
		return model.Page{}, fmt.Errorf("list exams: %w", err)
	}
	defer rows.Close()

	items := []model.Exam{}
	for rows.Next() {
		e, err := scanListRow(rows)
		if err != nil {
			return model.Page{}, err
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return model.Page{}, err
	}

	return model.Page{
		Items:      items,
		Total:      total,
		Page:       f.Page,
		PerPage:    f.PerPage,
		TotalPages: totalPages,
		HasPrev:    f.Page > 1,
		HasNext:    f.Page < totalPages,
	}, nil
}

// GetExam loads one exam including its body.
func (s *Store) GetExam(ctx context.Context, id int64) (model.Exam, error) {
	var e model.Exam
	err := s.pool.QueryRow(ctx, `
		SELECT e.id, e.source_key, e.external_id, e.title, e.url, e.summary, e.content,
		       e.category, e.region, e.published_at, e.published_raw, e.deadline_at,
		       e.list_hash, e.body_hash, e.is_read, e.first_seen_at, e.last_seen_at,
		       e.updated_at, COALESCE(s.name, e.source_key)
		FROM exams e
		LEFT JOIN sources s ON s.key = e.source_key
		WHERE e.id = $1`, id).
		Scan(&e.ID, &e.SourceKey, &e.ExternalID, &e.Title, &e.URL, &e.Summary, &e.Content,
			&e.Category, &e.Region, &e.PublishedAt, &e.PublishedRaw, &e.DeadlineAt,
			&e.ListHash, &e.BodyHash, &e.IsRead, &e.FirstSeenAt, &e.LastSeenAt,
			&e.UpdatedAt, &e.SourceName)
	if isNoRows(err) {
		return model.Exam{}, ErrNotFound
	}
	if err != nil {
		return model.Exam{}, fmt.Errorf("get exam: %w", err)
	}
	return e, nil
}

// ExamChanges returns the audit trail for one exam, newest first.
func (s *Store) ExamChanges(ctx context.Context, examID int64, limit int) ([]model.ExamChange, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, exam_id, run_id, change_type, changed_fields, created_at
		FROM exam_changes WHERE exam_id = $1
		ORDER BY created_at DESC LIMIT $2`, examID, limit)
	if err != nil {
		return nil, fmt.Errorf("exam changes: %w", err)
	}
	defer rows.Close()

	out := []model.ExamChange{}
	for rows.Next() {
		var c model.ExamChange
		if err := rows.Scan(&c.ID, &c.ExamID, &c.RunID, &c.ChangeType,
			&c.ChangedFields, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkRead sets the read flag on one exam.
func (s *Store) MarkRead(ctx context.Context, id int64, read bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE exams SET is_read = $2 WHERE id = $1`, id, read)
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead clears the unread flag everywhere and reports how many rows moved.
func (s *Store) MarkAllRead(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE exams SET is_read = true WHERE NOT is_read`)
	if err != nil {
		return 0, fmt.Errorf("mark all read: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Facet is a filter value with the number of exams behind it.
type Facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Facets holds the values offered by the filter controls.
type Facets struct {
	Categories []Facet `json:"categories"`
	Regions    []Facet `json:"regions"`
	Sources    []Facet `json:"sources"`
}

// Facets returns the distinct non-empty filter values, most populated first.
func (s *Store) Facets(ctx context.Context) (Facets, error) {
	var f Facets
	var err error
	if f.Categories, err = s.facetQuery(ctx,
		`SELECT category, count(*) FROM exams WHERE category <> '' GROUP BY category ORDER BY count(*) DESC, category`); err != nil {
		return f, err
	}
	if f.Regions, err = s.facetQuery(ctx,
		`SELECT region, count(*) FROM exams WHERE region <> '' GROUP BY region ORDER BY count(*) DESC, region`); err != nil {
		return f, err
	}
	if f.Sources, err = s.facetQuery(ctx, `
		SELECT e.source_key, count(*) FROM exams e
		GROUP BY e.source_key ORDER BY count(*) DESC, e.source_key`); err != nil {
		return f, err
	}
	return f, nil
}

func (s *Store) facetQuery(ctx context.Context, sql string) ([]Facet, error) {
	rows, err := s.pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("facets: %w", err)
	}
	defer rows.Close()

	out := []Facet{}
	for rows.Next() {
		var v Facet
		if err := rows.Scan(&v.Value, &v.Count); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
