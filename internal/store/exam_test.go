package store

import (
	"context"
	"strings"
	"testing"

	"github.com/BenLocal/exams/internal/model"
)

// ---------------------------------------------------------------------------
// Change detection
//
// This is the most load-bearing logic in the store. A false positive here
// turns into a stream of bogus "updated" notifications; a false negative means
// a genuinely revised deadline is never noticed.

func TestUpsertCreatesThenReportsUnchanged(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")

	first := upsert(t, st, "src", item("a", "公告 A"), item("b", "公告 B"))
	if first.Inserted != 2 || first.Updated != 0 || first.Unchanged != 0 {
		t.Fatalf("first pass: %+v, want 2 inserted", first)
	}
	if len(first.Created) != 2 {
		t.Errorf("Created = %d entries, want 2", len(first.Created))
	}

	// Re-crawling identical data must report nothing changed. Anything else
	// means every crawl notifies about every announcement.
	second := upsert(t, st, "src", item("a", "公告 A"), item("b", "公告 B"))
	if second.Inserted != 0 || second.Updated != 0 || second.Unchanged != 2 {
		t.Fatalf("second pass: %+v, want 2 unchanged", second)
	}
	if len(second.Created) != 0 || len(second.Changed) != 0 {
		t.Errorf("unchanged pass reported work: created=%d changed=%d",
			len(second.Created), len(second.Changed))
	}
	if n := countExams(t, st, "src"); n != 2 {
		t.Errorf("row count = %d, want 2 (re-crawling must not duplicate)", n)
	}
}

func TestUpsertReportsExactlyWhichFieldsChanged(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")

	base := item("a", "原标题")
	base.PublishedAt = at(3)
	base.DeadlineAt = at(0) // deadline 3 days after publication
	upsert(t, st, "src", base)

	// Change the deadline and the body, and nothing else.
	revised := item("a", "原标题")
	revised.PublishedAt = at(3)
	revised.DeadlineAt = at(-10)
	revised.Content = "正文改了"

	out := upsert(t, st, "src", revised)
	if out.Updated != 1 {
		t.Fatalf("expected 1 update, got %+v", out)
	}

	fields := changedFieldsOf(t, st, "a")
	assertSameSet(t, fields, []string{"deadline_at", "content"})

	// The audit row must be recorded against the run that made the change.
	var changeType string
	err := st.pool.QueryRow(context.Background(),
		`SELECT change_type FROM exam_changes ORDER BY id DESC LIMIT 1`).Scan(&changeType)
	if err != nil {
		t.Fatalf("read change: %v", err)
	}
	if changeType != model.ActionUpdated {
		t.Errorf("change_type = %q, want %q", changeType, model.ActionUpdated)
	}
}

func TestUpsertDoesNotResurrectReadOrResetFirstSeen(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")
	upsert(t, st, "src", item("a", "公告 A"))

	if err := st.MarkRead(context.Background(), examID(t, st, "a"), true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	var firstSeenBefore string
	if err := st.pool.QueryRow(context.Background(),
		`SELECT first_seen_at::text FROM exams WHERE external_id = 'a'`).Scan(&firstSeenBefore); err != nil {
		t.Fatalf("read first_seen_at: %v", err)
	}

	// A later crawl finds the text revised.
	revised := item("a", "公告 A（已修订）")
	upsert(t, st, "src", revised)

	var isRead bool
	var firstSeenAfter string
	if err := st.pool.QueryRow(context.Background(),
		`SELECT is_read, first_seen_at::text FROM exams WHERE external_id = 'a'`).
		Scan(&isRead, &firstSeenAfter); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if !isRead {
		t.Error("is_read was cleared by an update; a revised notice must not become unread again")
	}
	if firstSeenAfter != firstSeenBefore {
		t.Error("first_seen_at moved; it must record the first sighting, not the last")
	}
}

func TestUpsertWithoutBodyPreservesTheStoredOne(t *testing.T) {
	// A crawl only fetches a detail page when the list-level fields moved, so
	// most items arrive with no body. If that overwrote the stored body, every
	// unchanged item would slowly lose its content.
	st := testStore(t)
	seedSource(t, st, "src")

	withBody := item("a", "公告 A")
	withBody.Content = "这是已经抓到的正文。"
	upsert(t, st, "src", withBody)

	// A later crawl changes a list-level field but does not fetch the detail
	// page, so Content is empty.
	noBody := item("a", "公告 A")
	noBody.Content = ""
	noBody.Summary = "摘要有变动"
	out := upsert(t, st, "src", noBody)

	if out.Updated != 1 {
		t.Fatalf("expected the summary change to register, got %+v", out)
	}
	assertSameSet(t, changedFieldsOf(t, st, "a"), []string{"summary"})

	var content string
	if err := st.pool.QueryRow(context.Background(),
		`SELECT content FROM exams WHERE external_id = 'a'`).Scan(&content); err != nil {
		t.Fatalf("read content: %v", err)
	}
	if content != "这是已经抓到的正文。" {
		t.Errorf("content = %q, want the stored body preserved", content)
	}
	if strings.Contains(strings.Join(changedFieldsOf(t, st, "a"), ","), "content") {
		t.Error("content reported as changed even though no body was supplied")
	}
}

func TestAbsentBodyHashesToEmptyString(t *testing.T) {
	// sha256("") is a perfectly valid digest, so hashing an empty body would
	// make "body_hash <> ''" useless as a "do we have the body yet?" test —
	// and the crawl runner depends on exactly that test to decide whether to
	// retry a detail fetch.
	st := testStore(t)
	seedSource(t, st, "src")

	noBody := item("a", "没有正文")
	noBody.Content = ""
	upsert(t, st, "src", noBody)

	hashes, err := st.StoredHashes(context.Background(), "src")
	if err != nil {
		t.Fatalf("StoredHashes: %v", err)
	}
	if h := hashes["a"]; h.BodyHash != "" {
		t.Errorf("body_hash = %q for an empty body, want the empty string", h.BodyHash)
	}

	// Once a body arrives, the hash is populated and the runner stops
	// retrying.
	withBody := item("a", "没有正文")
	withBody.Content = "正文来了"
	upsert(t, st, "src", withBody)

	hashes, err = st.StoredHashes(context.Background(), "src")
	if err != nil {
		t.Fatalf("StoredHashes: %v", err)
	}
	if h := hashes["a"]; h.BodyHash == "" {
		t.Error("body_hash is still empty after a body was stored")
	}
}

func TestStoredHashesCarriesBothFingerprints(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")

	it := item("a", "公告 A")
	upsert(t, st, "src", it)

	hashes, err := st.StoredHashes(context.Background(), "src")
	if err != nil {
		t.Fatalf("StoredHashes: %v", err)
	}
	got, ok := hashes["a"]
	if !ok {
		t.Fatal("StoredHashes did not return the stored item")
	}
	// The runner compares these two against what a fresh crawl produced, so
	// they have to be the same values the collector computes.
	if got.ListHash != it.ListHash() {
		t.Errorf("list_hash = %q, want %q", got.ListHash, it.ListHash())
	}
	if got.BodyHash != it.BodyHash() {
		t.Errorf("body_hash = %q, want %q", got.BodyHash, it.BodyHash())
	}
}

func TestUpsertIsAtomic(t *testing.T) {
	// A batch has to commit or roll back as a unit: a failure part-way through
	// a source would otherwise leave a half-imported crawl behind, with
	// counters that do not match what is in the table.
	st := testStore(t)
	seedSource(t, st, "src")

	ctx := context.Background()
	mustExec(t, st, `
		CREATE OR REPLACE FUNCTION exams_test_reject() RETURNS trigger AS $$
		BEGIN
			IF NEW.title = 'BOOM' THEN
				RAISE EXCEPTION 'forced failure for the atomicity test';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER exams_test_reject_trg BEFORE INSERT OR UPDATE ON exams
			FOR EACH ROW EXECUTE FUNCTION exams_test_reject();`)
	t.Cleanup(func() {
		mustExec(t, st, `DROP TRIGGER IF EXISTS exams_test_reject_trg ON exams;
		                 DROP FUNCTION IF EXISTS exams_test_reject();`)
	})

	_, err := st.UpsertItems(ctx, "src", startRun(t, st, "src"), []model.Item{
		item("good-1", "正常公告一"),
		item("boom", "BOOM"),
		item("good-2", "正常公告二"),
	})
	if err == nil {
		t.Fatal("expected the batch to fail")
	}
	if n := countExams(t, st, "src"); n != 0 {
		t.Errorf("%d rows survived a failed batch; the transaction did not roll back", n)
	}
	var changes int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM exam_changes`).Scan(&changes); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if changes != 0 {
		t.Errorf("%d audit rows survived a failed batch", changes)
	}
}

// ---------------------------------------------------------------------------
// Listing, filtering and pagination

func TestListSearchCoversTextFieldsAndExactFacets(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")

	title := item("1", "关于考研报名的公告")
	title.PublishedAt = at(1)

	summary := item("2", "另一个标题")
	summary.Summary = "摘要里提到考研"
	summary.PublishedAt = at(2)

	body := item("3", "第三个标题")
	body.Content = "正文里提到考研"
	body.PublishedAt = at(3)

	// Matches only by category. This is the case that matters in practice:
	// people search for "考研", which is a category value, while the titles
	// say "全国硕士研究生招生考试".
	category := item("4", "全国硕士研究生招生考试公告")
	category.Category = "考研"
	category.PublishedAt = at(4)

	// Matches only by region.
	region := item("5", "广东省报名通知")
	region.Category = "四六级"
	region.Region = "广东"
	region.PublishedAt = at(5)

	// Matches nothing.
	unrelated := item("6", "无关公告")
	unrelated.Category = "其他"
	unrelated.Region = "北京"
	unrelated.Summary = "无关摘要"
	unrelated.Content = "无关正文"
	unrelated.PublishedAt = at(6)

	upsert(t, st, "src", title, summary, body, category, region, unrelated)

	got := searchIDs(t, st, model.ItemFilter{Query: "考研", PerPage: 50})
	assertSameSet(t, got, []string{"1", "2", "3", "4"})
}

func TestListSearchEscapesLikeMetacharacters(t *testing.T) {
	// Without escaping, searching for "%" would match every row and "_" would
	// match any single character.
	st := testStore(t)
	seedSource(t, st, "src")

	plain := item("plain", "普通公告")
	percent := item("pct", "通过率为 50% 的公告")
	underscore := item("und", "文件编号 A_B 的公告")

	upsert(t, st, "src", plain, percent, underscore)

	if got := searchIDs(t, st, model.ItemFilter{Query: "%", PerPage: 50}); len(got) != 1 || got[0] != "pct" {
		t.Errorf(`searching "%%" returned %v, want only the row containing a literal percent`, got)
	}
	if got := searchIDs(t, st, model.ItemFilter{Query: "A_B", PerPage: 50}); len(got) != 1 || got[0] != "und" {
		t.Errorf(`searching "A_B" returned %v, want only the exact match`, got)
	}
	if got := searchIDs(t, st, model.ItemFilter{Query: "A%F", PerPage: 50}); len(got) != 0 {
		t.Errorf(`searching "A%%F" returned %v, want nothing`, got)
	}
}

func TestListOrderIsATotalOrderSoPaginationIsSafe(t *testing.T) {
	// Government sites publish in batches on the same date, so many rows share
	// a published_at. An ORDER BY that stops there is not a total order, and
	// LIMIT/OFFSET over a partial order returns some rows twice and others
	// never — the classic pagination bug.
	//
	// What this actually guards, and what it does not:
	//
	//   - NULLS LAST is genuinely covered. Dropping it from the query fails
	//     this test.
	//   - The tiebreak assertion is a contract check, not regression coverage.
	//     exams_published_idx is declared (published_at DESC NULLS LAST, id
	//     DESC), so an index scan supplies the tiebreak even when the ORDER BY
	//     omits it — verified by mutation: deleting ", e.id DESC" from the
	//     query does not fail this test. Whether the ordering would then
	//     actually break depends on the plan and on PostgreSQL's sort being
	//     unstable, so the real failure cannot be reproduced deterministically
	//     here.
	//
	// The practical rule: the tiebreak belongs in BOTH the index and the query.
	// The index is what makes it fast; the ORDER BY is what makes it a
	// guarantee rather than a property of today's plan.
	st := testStore(t)
	seedSource(t, st, "src")

	// Every row shares one timestamp, plus two undated rows to pin down where
	// NULLs sort.
	items := make([]model.Item, 0, 25)
	for i := 0; i < 23; i++ {
		it := item(externalIDFor(i), "公告")
		it.PublishedAt = at(0)
		items = append(items, it)
	}
	undatedA := item("undated-a", "没有日期 A")
	undatedB := item("undated-b", "没有日期 B")
	upsert(t, st, "src", append(items, undatedA, undatedB)...)

	page, err := st.ListExams(context.Background(), model.ItemFilter{PerPage: 100})
	if err != nil {
		t.Fatalf("ListExams: %v", err)
	}
	if len(page.Items) != 25 {
		t.Fatalf("got %d rows, want 25", len(page.Items))
	}

	sawUndated := false
	for i := 1; i < len(page.Items); i++ {
		prev, cur := page.Items[i-1], page.Items[i]

		switch {
		case prev.PublishedAt == nil && cur.PublishedAt != nil:
			t.Errorf("row %d is undated but sorts before a dated row; NULLS LAST is not applied", i)
		case prev.PublishedAt != nil && cur.PublishedAt == nil:
			sawUndated = true
		case timeEqual(prev.PublishedAt, cur.PublishedAt):
			// The tiebreak. Without it these two rows have no defined order
			// relative to each other, and paging can repeat or skip them.
			if cur.ID >= prev.ID {
				t.Errorf("rows sharing a timestamp are not ordered by id: id %d then id %d; "+
					"the ORDER BY is not a total order", prev.ID, cur.ID)
			}
		default:
			if cur.PublishedAt.After(*prev.PublishedAt) {
				t.Errorf("row %d is newer than the row before it; ordering is not newest-first", i)
			}
		}
	}
	if !sawUndated {
		t.Error("undated rows never appeared at the end; NULLS LAST is not being applied")
	}
}

func externalIDFor(i int) string {
	return "row-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}

func TestListClampsAnOutOfRangePage(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")

	items := make([]model.Item, 0, 5)
	for i := 0; i < 5; i++ {
		it := item(string(rune('a'+i)), "公告")
		it.PublishedAt = at(i)
		items = append(items, it)
	}
	upsert(t, st, "src", items...)

	// A stale bookmark, or a filter that shrank the result set, lands here.
	// Returning an empty list would read as "nothing matched".
	page, err := st.ListExams(context.Background(), model.ItemFilter{Page: 99, PerPage: 2})
	if err != nil {
		t.Fatalf("ListExams: %v", err)
	}
	if page.Page != page.TotalPages {
		t.Errorf("page = %d, want it clamped to the last page %d", page.Page, page.TotalPages)
	}
	if len(page.Items) == 0 {
		t.Error("clamped page returned no items")
	}
	if page.HasNext {
		t.Error("HasNext is true on the last page")
	}
}

func TestListOrdersNewestFirst(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")

	old := item("old", "旧公告")
	old.PublishedAt = at(10)
	mid := item("mid", "中间公告")
	mid.PublishedAt = at(5)
	newest := item("new", "新公告")
	newest.PublishedAt = at(1)
	undated := item("undated", "没有日期")
	upsert(t, st, "src", old, mid, newest, undated)

	page, err := st.ListExams(context.Background(), model.ItemFilter{PerPage: 10})
	if err != nil {
		t.Fatalf("ListExams: %v", err)
	}
	var ids []string
	for _, e := range page.Items {
		ids = append(ids, e.ExternalID)
	}
	want := []string{"new", "mid", "old", "undated"} // NULLS LAST
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", ids, want)
	}
}

func TestListUnreadFilterAndMarkAllRead(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")
	upsert(t, st, "src", item("a", "公告 A"), item("b", "公告 B"))

	page, err := st.ListExams(context.Background(), model.ItemFilter{Unread: true, PerPage: 10})
	if err != nil {
		t.Fatalf("ListExams: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("unread total = %d, want 2", page.Total)
	}

	n, err := st.MarkAllRead(context.Background())
	if err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	if n != 2 {
		t.Errorf("MarkAllRead reported %d rows, want 2", n)
	}

	page, err = st.ListExams(context.Background(), model.ItemFilter{Unread: true, PerPage: 10})
	if err != nil {
		t.Fatalf("ListExams: %v", err)
	}
	if page.Total != 0 {
		t.Errorf("unread total = %d after marking all read, want 0", page.Total)
	}
}

func TestGetExamReturnsBodyAndNotFound(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")
	upsert(t, st, "src", item("a", "公告 A"))

	got, err := st.GetExam(context.Background(), examID(t, st, "a"))
	if err != nil {
		t.Fatalf("GetExam: %v", err)
	}
	if got.Content == "" {
		t.Error("GetExam returned an empty body; the detail page would be blank")
	}

	if _, err := st.GetExam(context.Background(), 999999); err != ErrNotFound {
		t.Errorf("missing exam: err = %v, want ErrNotFound", err)
	}
}

func TestListDoesNotCarryBodies(t *testing.T) {
	// The list template only renders the summary; pulling every body through
	// the wire would grow with the table.
	st := testStore(t)
	seedSource(t, st, "src")
	upsert(t, st, "src", item("a", "公告 A"))

	page, err := st.ListExams(context.Background(), model.ItemFilter{PerPage: 10})
	if err != nil {
		t.Fatalf("ListExams: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d items", len(page.Items))
	}
	if page.Items[0].Content != "" {
		t.Error("list query returned a body; it should select only list columns")
	}
}

func TestStatsAndFacets(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")

	// item() sets a category and a region by default, so each facet is cleared
	// on the row that is not meant to contribute to it.
	cat := item("a", "公告 A")
	cat.Category = "公务员"
	cat.Region = ""

	reg := item("b", "公告 B")
	reg.Category = ""
	reg.Region = "广东"

	upsert(t, st, "src", cat, reg)

	stats, err := st.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalExams != 2 || stats.UnreadExams != 2 || stats.SourceCount != 1 {
		t.Errorf("stats = %+v", stats)
	}

	facets, err := st.Facets(context.Background())
	if err != nil {
		t.Fatalf("Facets: %v", err)
	}
	if len(facets.Categories) != 1 || facets.Categories[0].Value != "公务员" {
		t.Errorf("categories = %+v", facets.Categories)
	}
	if len(facets.Regions) != 1 || facets.Regions[0].Value != "广东" {
		t.Errorf("regions = %+v", facets.Regions)
	}
	if len(facets.Sources) != 1 || facets.Sources[0].Count != 2 {
		t.Errorf("sources = %+v", facets.Sources)
	}
}

// ---------------------------------------------------------------------------
// Helpers

func examID(t *testing.T, st *Store, externalID string) int64 {
	t.Helper()
	var id int64
	err := st.pool.QueryRow(context.Background(),
		`SELECT id FROM exams WHERE external_id = $1`, externalID).Scan(&id)
	if err != nil {
		t.Fatalf("look up exam %q: %v", externalID, err)
	}
	return id
}

func changedFieldsOf(t *testing.T, st *Store, externalID string) []string {
	t.Helper()
	var fields []string
	err := st.pool.QueryRow(context.Background(), `
		SELECT changed_fields FROM exam_changes
		WHERE exam_id = (SELECT id FROM exams WHERE external_id = $1)
		ORDER BY id DESC LIMIT 1`, externalID).Scan(&fields)
	if err != nil {
		t.Fatalf("read changed_fields for %q: %v", externalID, err)
	}
	return fields
}

func searchIDs(t *testing.T, st *Store, filter model.ItemFilter) []string {
	t.Helper()
	page, err := st.ListExams(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListExams(%+v): %v", filter, err)
	}
	ids := make([]string, 0, len(page.Items))
	for _, e := range page.Items {
		ids = append(ids, e.ExternalID)
	}
	return ids
}

func assertSameSet(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	counts := map[string]int{}
	for _, v := range got {
		counts[v]++
	}
	for _, v := range want {
		counts[v]--
	}
	for v, n := range counts {
		if n != 0 {
			t.Fatalf("got %v, want %v (off by %d on %q)", got, want, n, v)
		}
	}
}

func mustExec(t *testing.T, st *Store, sql string) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec: %v\n%s", err, sql)
	}
}
