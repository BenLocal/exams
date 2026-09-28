package store

import (
	"context"
	"testing"

	"github.com/BenLocal/exams/internal/model"
)

// ---------------------------------------------------------------------------
// Notification ledger

func TestNotificationIsDeliveredOnce(t *testing.T) {
	st := testStore(t)
	seedSource(t, st, "src")
	upsert(t, st, "src", item("a", "公告 A"))
	id := examID(t, st, "a")
	ctx := context.Background()

	claimID, ok, err := st.ClaimNotification(ctx, id, "email", "ops@example.test", model.ActionCreated)
	if err != nil {
		t.Fatalf("ClaimNotification: %v", err)
	}
	if !ok {
		t.Fatal("first claim was refused")
	}
	if err := st.RecordNotificationResult(ctx, claimID, true, ""); err != nil {
		t.Fatalf("RecordNotificationResult: %v", err)
	}

	// Re-running the crawl must not send the same notification again.
	if _, ok, err := st.ClaimNotification(ctx, id, "email", "ops@example.test", model.ActionCreated); err != nil {
		t.Fatalf("second claim: %v", err)
	} else if ok {
		t.Error("a delivered notification was claimed again; the ledger is not deduplicating")
	}
}

func TestFailedNotificationIsRetried(t *testing.T) {
	// The whole point of treating the ledger as a delivery log rather than a
	// "seen" set: a transient SMTP failure must not suppress the notification
	// forever.
	st := testStore(t)
	seedSource(t, st, "src")
	upsert(t, st, "src", item("a", "公告 A"))
	id := examID(t, st, "a")
	ctx := context.Background()

	claimID, ok, err := st.ClaimNotification(ctx, id, "webhook", "https://hook.test/x", model.ActionCreated)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if err := st.RecordNotificationResult(ctx, claimID, false, "connection refused"); err != nil {
		t.Fatalf("RecordNotificationResult: %v", err)
	}

	retryID, ok, err := st.ClaimNotification(ctx, id, "webhook", "https://hook.test/x", model.ActionCreated)
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if !ok {
		t.Fatal("a failed notification was not offered for retry")
	}

	var attempts int
	if err := st.pool.QueryRow(ctx,
		`SELECT attempts FROM notifications WHERE id = $1`, retryID).Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}

	// After it succeeds, retries stop.
	if err := st.RecordNotificationResult(ctx, retryID, true, ""); err != nil {
		t.Fatalf("record success: %v", err)
	}
	if _, ok, _ := st.ClaimNotification(ctx, id, "webhook", "https://hook.test/x", model.ActionCreated); ok {
		t.Error("a notification was claimed again after it succeeded")
	}
}

func TestNotificationTargetsAreIndependent(t *testing.T) {
	// One unreachable recipient must not block the others.
	st := testStore(t)
	seedSource(t, st, "src")
	upsert(t, st, "src", item("a", "公告 A"))
	id := examID(t, st, "a")
	ctx := context.Background()

	if _, ok, _ := st.ClaimNotification(ctx, id, "email", "a@example.test", model.ActionCreated); !ok {
		t.Fatal("first target was refused")
	}
	if _, ok, err := st.ClaimNotification(ctx, id, "email", "b@example.test", model.ActionCreated); err != nil || !ok {
		t.Fatalf("second target: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.ClaimNotification(ctx, id, "webhook", "https://hook.test/x", model.ActionCreated); err != nil || !ok {
		t.Fatalf("different channel: ok=%v err=%v", ok, err)
	}

	sent, failed, err := st.NotificationCounts(ctx)
	if err != nil {
		t.Fatalf("NotificationCounts: %v", err)
	}
	if sent != 0 || failed != 0 {
		t.Errorf("counts = sent:%d failed:%d, want 0/0 while everything is pending", sent, failed)
	}
}

// ---------------------------------------------------------------------------
// Source registry

func TestSyncSourcesDoesNotClobberOperatorChoices(t *testing.T) {
	// `enabled` and `interval` are set by the operator through the UI. A
	// restart re-syncs the registry, and must not silently undo them.
	st := testStore(t)
	ctx := context.Background()

	seedSource(t, st, "src")
	if err := st.SetSourceEnabled(ctx, "src", false); err != nil {
		t.Fatalf("SetSourceEnabled: %v", err)
	}
	if err := st.SetSourceInterval(ctx, "src", "0 3 * * *"); err != nil {
		t.Fatalf("SetSourceInterval: %v", err)
	}

	// A restart with a renamed collector.
	if err := st.SyncSources(ctx, []model.Source{
		{Key: "src", Name: "改了名字", BaseURL: "https://moved.test"},
	}); err != nil {
		t.Fatalf("SyncSources: %v", err)
	}

	got, err := st.GetSource(ctx, "src")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if got.Name != "改了名字" || got.BaseURL != "https://moved.test" {
		t.Errorf("descriptive fields were not refreshed: %+v", got)
	}
	if got.Enabled {
		t.Error("disabled source was re-enabled by a restart")
	}
	if got.Interval != "0 3 * * *" {
		t.Errorf("interval = %q, want the operator's value preserved", got.Interval)
	}
}

func TestSyncSourcesKeepsOrphanRows(t *testing.T) {
	// A row whose collector no longer exists in the binary must survive, so
	// its scraped announcements stay reachable.
	st := testStore(t)
	ctx := context.Background()

	seedSource(t, st, "removed-collector")
	seedSource(t, st, "still-here")

	sources, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(sources))
	}
}

func TestSourceMutationsReportMissingRows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if err := st.SetSourceEnabled(ctx, "nope", true); err == nil {
		t.Error("SetSourceEnabled accepted an unknown key")
	}
	if _, err := st.GetSource(ctx, "nope"); err == nil {
		t.Error("GetSource accepted an unknown key")
	}
}

func TestMarkSourceRunSetsBootstrapOnce(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seedSource(t, st, "src")

	src, _ := st.GetSource(ctx, "src")
	if src.BootstrapDone {
		t.Fatal("a fresh source is already marked bootstrapped")
	}

	if err := st.MarkSourceRun(ctx, "src", at(0).UTC(), true); err != nil {
		t.Fatalf("MarkSourceRun: %v", err)
	}
	src, _ = st.GetSource(ctx, "src")
	if !src.BootstrapDone {
		t.Error("bootstrap_done was not set")
	}
	if src.LastRunAt == nil {
		t.Error("last_run_at was not set")
	}

	// Passing false must not clear it: the point is that the backlog has been
	// seen at least once.
	if err := st.MarkSourceRun(ctx, "src", at(0).UTC(), false); err != nil {
		t.Fatalf("MarkSourceRun: %v", err)
	}
	src, _ = st.GetSource(ctx, "src")
	if !src.BootstrapDone {
		t.Error("bootstrap_done was cleared")
	}
}

// ---------------------------------------------------------------------------
// Runs and locking

func TestTryLockIsExclusivePerSource(t *testing.T) {
	// The in-process mutex only guards one process. This lock is what stops a
	// hand-run `exams collect` from colliding with a live server, so it has to
	// be genuinely exclusive.
	st := testStore(t)
	ctx := context.Background()

	release, ok, err := st.TryLock(ctx, "src")
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if !ok {
		t.Fatal("first lock was refused")
	}

	if _, ok, err := st.TryLock(ctx, "src"); err != nil {
		t.Fatalf("second TryLock: %v", err)
	} else if ok {
		t.Error("the same source was locked twice")
	}

	// A different source is unaffected.
	otherRelease, ok, err := st.TryLock(ctx, "other")
	if err != nil {
		t.Fatalf("TryLock(other): %v", err)
	}
	if !ok {
		t.Error("locking one source blocked a different source")
	}
	otherRelease()

	release()
	releaseAgain, ok, err := st.TryLock(ctx, "src")
	if err != nil {
		t.Fatalf("TryLock after release: %v", err)
	}
	if !ok {
		t.Error("the lock was not released")
	}
	releaseAgain()
}

func TestReapStaleRuns(t *testing.T) {
	// A process killed mid-crawl leaves a 'running' row behind. Without this,
	// the runs page shows a crawl that has been in progress for days.
	st := testStore(t)
	ctx := context.Background()

	runID, err := st.StartRun(ctx, "src")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	n, err := st.ReapStaleRuns(ctx)
	if err != nil {
		t.Fatalf("ReapStaleRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped %d rows, want 1", n)
	}

	runs, err := st.ListRuns(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var found bool
	for _, r := range runs {
		if r.ID == runID {
			found = true
			if r.Status != model.StatusFailed {
				t.Errorf("status = %q, want %q", r.Status, model.StatusFailed)
			}
			if r.FinishedAt == nil {
				t.Error("finished_at is still NULL on a reaped run")
			}
			if r.Error == "" {
				t.Error("a reaped run should say why it was closed")
			}
		}
	}
	if !found {
		t.Fatal("the reaped run disappeared from the list")
	}
}

func TestReapStaleRunsLeavesFinishedRunsAlone(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	okRun, _ := st.StartRun(ctx, "src")
	if err := st.FinishRun(ctx, okRun, RunResult{Status: model.StatusSuccess, Fetched: 3}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	if n, err := st.ReapStaleRuns(ctx); err != nil || n != 0 {
		t.Fatalf("reaped %d finished runs (err=%v), want 0", n, err)
	}
}

func TestFinishRunRejectsNonTerminalStatus(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	runID, _ := st.StartRun(ctx, "src")

	if err := st.FinishRun(ctx, runID, RunResult{Status: model.StatusRunning}); err == nil {
		t.Error("FinishRun accepted 'running'; a run could be closed without ever finishing")
	}
}

func TestListRunsFiltersBySourceAndOrdersNewestFirst(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	seedSource(t, st, "src")
	seedSource(t, st, "other")

	first, _ := st.StartRun(ctx, "src")
	_ = st.FinishRun(ctx, first, RunResult{Status: model.StatusSuccess})
	second, _ := st.StartRun(ctx, "other")
	_ = st.FinishRun(ctx, second, RunResult{Status: model.StatusFailed, ErrMsg: "网络超时"})

	all, err := st.ListRuns(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d runs, want 2", len(all))
	}
	if all[0].SourceKey != "other" {
		t.Errorf("newest run is %q, want the most recent one first", all[0].SourceKey)
	}
	if all[0].SourceName == "" {
		t.Error("source name was not joined in; the runs page would show a bare key")
	}
	if all[0].Error != "网络超时" {
		t.Errorf("error = %q, want the recorded failure preserved", all[0].Error)
	}

	only, err := st.ListRuns(ctx, "src", 10)
	if err != nil {
		t.Fatalf("ListRuns(src): %v", err)
	}
	if len(only) != 1 || only[0].SourceKey != "src" {
		t.Errorf("filter returned %+v", only)
	}
}

func TestStatsCountsFailingSources(t *testing.T) {
	// "Why is there no data?" should be answerable from the top of the list
	// page, so a source whose latest run failed or came back empty counts.
	st := testStore(t)
	ctx := context.Background()
	seedSource(t, st, "broken")
	seedSource(t, st, "empty")
	seedSource(t, st, "fine")

	failed, _ := st.StartRun(ctx, "broken")
	_ = st.FinishRun(ctx, failed, RunResult{Status: model.StatusFailed, ErrMsg: "连接超时"})

	empty, _ := st.StartRun(ctx, "empty")
	_ = st.FinishRun(ctx, empty, RunResult{Status: model.StatusEmpty})

	// The healthy source fails first, then recovers: only the latest run
	// should count.
	old, _ := st.StartRun(ctx, "fine")
	_ = st.FinishRun(ctx, old, RunResult{Status: model.StatusFailed})
	recent, _ := st.StartRun(ctx, "fine")
	_ = st.FinishRun(ctx, recent, RunResult{Status: model.StatusSuccess})

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.FailingSources != 2 {
		t.Errorf("failing sources = %d, want 2 (broken and empty, not the recovered one)",
			stats.FailingSources)
	}
}

// ---------------------------------------------------------------------------
// Migrations

func TestMigrateIsIdempotent(t *testing.T) {
	st := testStore(t) // already migrated once

	results, err := st.Migrate(context.Background())
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Migrate returned no results")
	}
	for _, r := range results {
		if r.Applied {
			t.Errorf("%s was applied a second time", r.Version)
		}
		if !r.Skipped {
			t.Errorf("%s: skipped=%v applied=%v err=%v", r.Version, r.Skipped, r.Applied, r.Err)
		}
	}
}

func TestPendingRequiredIsEmptyAfterMigrating(t *testing.T) {
	st := testStore(t)

	pending, err := st.PendingRequired(context.Background())
	if err != nil {
		t.Fatalf("PendingRequired: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v after a successful migrate, want none", pending)
	}
}
