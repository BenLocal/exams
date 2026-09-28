// Package crawl orchestrates one crawl of one source: fetch, enrich, store,
// record, notify.
package crawl

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/BenLocal/exams/internal/collector"
	"github.com/BenLocal/exams/internal/model"
	"github.com/BenLocal/exams/internal/notify"
	"github.com/BenLocal/exams/internal/store"
)

// Options tunes a Runner.
type Options struct {
	// MaxItems caps how many items one crawl will store, so a selector that
	// suddenly matches the whole site cannot flood the database.
	MaxItems int
	// MaxDetails caps how many detail pages one crawl will fetch. A source's
	// first crawl sees the whole backlog; without a cap that becomes hundreds
	// of sequential requests. Anything deferred is picked up on the next run.
	MaxDetails int
	// Timeout bounds the network phase of a crawl.
	Timeout time.Duration
	// NotifyOnUpdate also notifies when an existing announcement changes,
	// not only when a new one appears.
	NotifyOnUpdate bool
}

// Runner executes crawls.
type Runner struct {
	store      *store.Store
	fetcher    *collector.Fetcher
	log        *slog.Logger
	opts       Options
	dispatcher *notify.Dispatcher
}

// New builds a Runner. dispatcher may be nil, in which case nothing is sent.
func New(st *store.Store, f *collector.Fetcher, log *slog.Logger, opts Options, d *notify.Dispatcher) *Runner {
	if log == nil {
		log = slog.Default()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	return &Runner{store: st, fetcher: f, log: log, opts: opts, dispatcher: d}
}

// RunSource crawls one source and returns the recorded run.
//
// A returned error means the run could not be recorded at all; a crawl that
// failed on the network returns a non-nil CrawlRun with status "failed" and a
// nil error, because "the crawl failed" is a normal outcome that belongs on
// the runs page rather than in a log line.
func (r *Runner) RunSource(ctx context.Context, key string) (model.CrawlRun, error) {
	c, ok := collector.Get(key)
	if !ok {
		return model.CrawlRun{}, fmt.Errorf("no collector registered for %q (registered: %s)",
			key, strings.Join(collector.Keys(), ", "))
	}

	// Read the source first: whether it has ever completed a run decides
	// whether this run is allowed to notify.
	src, srcErr := r.store.GetSource(ctx, key)
	if srcErr != nil {
		r.log.Warn("source row not found; run will not be attributed", "source", key, "err", srcErr)
	}

	runID, err := r.store.StartRun(ctx, key)
	if err != nil {
		return model.CrawlRun{}, err
	}

	startedAt := time.Now()
	run := model.CrawlRun{
		ID: runID, SourceKey: key, Status: model.StatusRunning, StartedAt: startedAt,
	}

	// The network phase gets its own timeout. The final write deliberately does
	// not, so a crawl that times out still records why.
	netCtx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	result, outcome := r.collect(netCtx, c, key, runID)
	cancel()

	// context.WithoutCancel lets the terminal status land even while the
	// process is shutting down.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer finishCancel()

	if err := r.store.FinishRun(finishCtx, runID, result); err != nil {
		r.log.Error("could not record run result", "source", key, "run", runID, "err", err)
	}

	finishedAt := time.Now()
	run.Status = result.Status
	run.FinishedAt = &finishedAt
	run.Fetched, run.Inserted, run.Updated, run.Unchanged = result.Fetched, result.Inserted, result.Updated, result.Unchanged
	run.Error, run.Note = result.ErrMsg, result.Note
	run.DurationMS = finishedAt.Sub(startedAt).Milliseconds()

	// A source is bootstrapped once it has completed any run, successful or
	// not — the point is that its backlog has been seen.
	if err := r.store.MarkSourceRun(finishCtx, key, finishedAt, true); err != nil {
		r.log.Error("could not update source last_run_at", "source", key, "err", err)
	}

	// First successful crawl of a source imports its whole backlog. Notifying
	// on that would fire hundreds of messages at the moment a source is added.
	if !src.BootstrapDone && result.Status == model.StatusSuccess && len(outcome.Created) > 0 {
		r.log.Info("suppressing notifications for a source's first run",
			"source", key, "count", len(outcome.Created))
	} else if result.Status == model.StatusSuccess {
		r.notifyAsync(key, src.Name, outcome)
	}

	return run, nil
}

// collect performs the fetch/store phase and classifies the outcome.
func (r *Runner) collect(ctx context.Context, c collector.Collector, key string, runID int64) (store.RunResult, model.UpsertOutcome) {
	items, err := c.List(ctx, r.fetcher)
	if err != nil {
		return store.RunResult{Status: model.StatusFailed, ErrMsg: err.Error()}, model.UpsertOutcome{}
	}

	note := ""
	if r.opts.MaxItems > 0 && len(items) > r.opts.MaxItems {
		note = fmt.Sprintf("truncated to CRAWL_MAX_ITEMS=%d from %d parsed items",
			r.opts.MaxItems, len(items))
		r.log.Warn("item cap reached", "source", key, "parsed", len(items), "cap", r.opts.MaxItems)
		items = items[:r.opts.MaxItems]
	}

	fetched := len(items)

	if d, ok := c.(collector.Detailer); ok {
		items = r.fillDetails(ctx, key, d, items)
	}

	outcome, err := r.store.UpsertItems(ctx, key, runID, items)
	if err != nil {
		return store.RunResult{
			Status: model.StatusFailed, Fetched: fetched, ErrMsg: err.Error(), Note: note,
		}, outcome
	}

	status := model.StatusSuccess
	if fetched == 0 {
		// Zero items from a successful fetch is the signature of a broken
		// selector or a challenge page, not a quiet news day. Recording it as
		// "empty" keeps it visible on the runs page.
		status = model.StatusEmpty
		note = joinNote(note, "fetched 0 items: check that the selectors still match the live page")
	}

	return store.RunResult{
		Status:    status,
		Fetched:   fetched,
		Inserted:  outcome.Inserted,
		Updated:   outcome.Updated,
		Unchanged: outcome.Unchanged,
		Note:      note,
	}, outcome
}

// fillDetails fetches detail pages only where they are actually needed.
//
// An item's body is skipped when its list-level fields are unchanged AND a
// body is already on file. That is what keeps a steady-state crawl at roughly
// one request per source instead of one per announcement.
//
// The pass is capped at opts.MaxDetails per run so that a source's first crawl
// — which sees the entire backlog at once — cannot turn into hundreds of
// sequential requests and trip a rate limit or the crawl timeout. Items left
// over keep an empty body hash, so the next run picks them up.
func (r *Runner) fillDetails(ctx context.Context, key string, d collector.Detailer, items []model.Item) []model.Item {
	stored, err := r.store.StoredHashes(ctx, key)
	if err != nil {
		r.log.Warn("could not load stored hashes; fetching every detail page",
			"source", key, "err", err)
		stored = map[string]store.StoredHash{}
	}

	var skipped, fetched, failed, capped int
	for i := range items {
		if ctx.Err() != nil {
			r.log.Warn("detail pass cut short by timeout", "source", key, "remaining", len(items)-i)
			break
		}

		if prev, ok := stored[items[i].ExternalID]; ok &&
			prev.ListHash == items[i].ListHash() && prev.BodyHash != "" {
			// Nothing on the list page moved and the body is already stored.
			skipped++
			continue
		}

		if r.opts.MaxDetails > 0 && fetched >= r.opts.MaxDetails {
			// Deliberately not counted as failed: these are picked up next run.
			capped = len(items) - i
			break
		}

		detailed, err := d.Detail(ctx, r.fetcher, items[i])
		if err != nil {
			// One unreachable detail page must not fail the whole source. The
			// item is stored with whatever the list page gave us, and because
			// its body hash stays empty it is retried on the next run.
			failed++
			r.log.Warn("detail fetch failed", "source", key, "url", items[i].URL, "err", err)
			continue
		}
		items[i] = detailed
		fetched++
	}

	r.log.Info("detail pass complete",
		"source", key, "total", len(items), "fetched", fetched,
		"unchanged_skipped", skipped, "failed", failed, "deferred_to_next_run", capped)
	return items
}

// notifyAsync hands the events to the dispatcher in the background.
//
// Notifications must never extend a crawl: a slow SMTP server or a hanging
// webhook would otherwise hold a database connection and a worker slot.
func (r *Runner) notifyAsync(sourceKey, sourceName string, outcome model.UpsertOutcome) {
	if r.dispatcher == nil || !r.dispatcher.Enabled() {
		return
	}

	events := make([]notify.Event, 0, len(outcome.Created)+len(outcome.Changed))
	for _, e := range outcome.Created {
		events = append(events, eventFrom(e, sourceName, model.ActionCreated))
	}
	if r.opts.NotifyOnUpdate {
		for _, e := range outcome.Changed {
			events = append(events, eventFrom(e, sourceName, model.ActionUpdated))
		}
	}
	if len(events) == 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		r.dispatcher.Dispatch(ctx, events)
	}()
}

func eventFrom(e model.Exam, sourceName, action string) notify.Event {
	return notify.Event{
		ExamID:     e.ID,
		SourceKey:  e.SourceKey,
		SourceName: sourceName,
		Action:     action,
		Title:      e.Title,
		URL:        e.URL,
		Summary:    e.Summary,
		Category:   e.Category,
		Region:     e.Region,
	}
}

func joinNote(existing, extra string) string {
	if existing == "" {
		return extra
	}
	return existing + "; " + extra
}
