// Package scheduler decides when each source is crawled and runs it without
// overlapping itself.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BenLocal/exams/internal/collector"
	"github.com/BenLocal/exams/internal/crawl"
	"github.com/BenLocal/exams/internal/model"
	"github.com/BenLocal/exams/internal/store"
	"github.com/robfig/cron/v3"
)

// Options configures the scheduler.
type Options struct {
	// GlobalCron is the default schedule for sources with no interval of their
	// own.
	GlobalCron string
	// RunOnStart crawls every due source immediately at startup, so a fresh
	// install populates without waiting for the first tick.
	RunOnStart bool
	// Concurrency caps how many sources crawl at once.
	Concurrency int
}

// Scheduler owns the crawl loop.
//
// It uses a single cron entry that fires every minute and asks the database
// which sources are due, rather than one cron entry per source. Sources are
// enabled, disabled and re-scheduled through the web UI, and per-source
// entries would need rebuilding on every such change — a whole class of
// "the toggle did not take effect" bugs that this design does not have.
type Scheduler struct {
	store    *store.Store
	runner   *crawl.Runner
	log      *slog.Logger
	opts     Options
	location *time.Location

	cron    *cron.Cron
	trigger chan string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// running holds source keys with an in-flight crawl, so a slow source
	// skips its next slot instead of queueing up behind itself.
	running sync.Map

	schedMu sync.Mutex
	scheds  map[string]cron.Schedule
}

// New builds a Scheduler. Start must be called to begin crawling.
func New(st *store.Store, r *crawl.Runner, log *slog.Logger, opts Options, loc *time.Location) (*Scheduler, error) {
	if log == nil {
		log = slog.Default()
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if loc == nil {
		loc = time.UTC
	}
	if _, err := parseSpec(opts.GlobalCron); err != nil {
		return nil, fmt.Errorf("CRAWL_CRON %q: %w", opts.GlobalCron, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		store:    st,
		runner:   r,
		log:      log,
		opts:     opts,
		location: loc,
		cron:     cron.New(cron.WithLocation(loc)),
		// Buffer enough for every source to be enqueued at once.
		trigger: make(chan string, 256),
		ctx:     ctx,
		cancel:  cancel,
		scheds:  map[string]cron.Schedule{},
	}, nil
}

// Start registers the tick, reaps interrupted runs from a previous process,
// and launches the workers.
func (s *Scheduler) Start(ctx context.Context) error {
	// A run left in 'running' belongs to a process that died. Close it out
	// before anything new starts, so the runs page is never permanently wrong.
	reaped, err := s.store.ReapStaleRuns(ctx)
	if err != nil {
		return err
	}
	if reaped > 0 {
		s.log.Warn("closed out runs interrupted by a previous shutdown", "count", reaped)
	}

	if _, err := s.cron.AddFunc("* * * * *", func() { s.dispatchDue() }); err != nil {
		return fmt.Errorf("register tick: %w", err)
	}

	go s.worker()
	s.cron.Start()

	if s.opts.RunOnStart {
		go s.runStartupPass()
	}
	return nil
}

// runStartupPass queues sources that are due, plus any that have never run.
func (s *Scheduler) runStartupPass() {
	// Give the listener a moment to come up before competing for resources.
	select {
	case <-time.After(2 * time.Second):
	case <-s.ctx.Done():
		return
	}
	s.dispatchDueIncluding(s.ctx, true)
}

// dispatchDue queues every enabled source whose next scheduled time has passed.
func (s *Scheduler) dispatchDue() {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	s.dispatchDueIncluding(ctx, false)
}

func (s *Scheduler) dispatchDueIncluding(ctx context.Context, includeNeverRun bool) {
	sources, err := s.store.ListSources(ctx)
	if err != nil {
		s.log.Error("could not list sources", "err", err)
		return
	}

	now := time.Now().In(s.location)
	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		// A row whose collector no longer exists in the binary is shown on the
		// sources page but never crawled. The registry is consulted directly
		// rather than trusting model.Source.Registered, which is a display-only
		// annotation the store never populates.
		if _, ok := collector.Get(src.Key); !ok {
			continue
		}
		if s.isDue(src, now, includeNeverRun) {
			s.Trigger(src.Key)
		}
	}
}

// isDue reports whether a source should run now.
func (s *Scheduler) isDue(src model.Source, now time.Time, includeNeverRun bool) bool {
	spec := src.Interval
	if spec == "" {
		spec = s.opts.GlobalCron
	}
	sched, err := s.schedule(spec)
	if err != nil {
		s.log.Error("invalid cron spec for source", "source", src.Key, "spec", spec, "err", err)
		return false
	}

	base := src.CreatedAt
	if src.LastRunAt != nil {
		base = *src.LastRunAt
	} else if includeNeverRun {
		return true
	}
	return !sched.Next(base.In(s.location)).After(now)
}

// schedule parses and caches a cron spec.
func (s *Scheduler) schedule(spec string) (cron.Schedule, error) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if sched, ok := s.scheds[spec]; ok {
		return sched, nil
	}
	sched, err := parseSpec(spec)
	if err != nil {
		return nil, err
	}
	s.scheds[spec] = sched
	return sched, nil
}

func parseSpec(spec string) (cron.Schedule, error) {
	if spec == "" {
		return nil, errors.New("empty cron spec")
	}
	return cron.ParseStandard(spec)
}

// Trigger queues a crawl. It never blocks and reports whether the source was
// enqueued.
func (s *Scheduler) Trigger(key string) bool {
	select {
	case s.trigger <- key:
		return true
	case <-s.ctx.Done():
		return false
	default:
		s.log.Warn("trigger queue full; dropping request", "source", key)
		return false
	}
}

// worker consumes the trigger queue and runs crawls under a concurrency limit.
func (s *Scheduler) worker() {
	sem := make(chan struct{}, s.opts.Concurrency)
	for {
		select {
		case <-s.ctx.Done():
			return
		case key := <-s.trigger:
			if _, busy := s.running.LoadOrStore(key, struct{}{}); busy {
				// A crawl that outruns its interval should be skipped, never
				// queued: catching up on missed slots is never what is wanted.
				s.log.Warn("skipping run, previous run still in progress", "source", key)
				continue
			}

			select {
			case sem <- struct{}{}:
			case <-s.ctx.Done():
				s.running.Delete(key)
				return
			}

			s.wg.Add(1)
			go func(k string) {
				defer func() {
					<-sem
					s.running.Delete(k)
					s.wg.Done()
				}()
				s.runOne(s.ctx, k)
			}(key)
		}
	}
}

// runOne crawls a single source under a cross-process lock.
func (s *Scheduler) runOne(ctx context.Context, key string) {
	release, ok, err := s.store.TryLock(ctx, key)
	if err != nil {
		s.log.Error("could not take crawl lock", "source", key, "err", err)
		return
	}
	if !ok {
		// Another process — typically a hand-run `exams collect` — holds it.
		s.log.Warn("skipping run, another process holds the lock for this source", "source", key)
		return
	}
	defer release()

	run, err := s.runner.RunSource(ctx, key)
	if err != nil {
		s.log.Error("crawl could not be recorded", "source", key, "err", err)
		return
	}

	switch run.Status {
	case model.StatusFailed:
		s.log.Error("crawl failed", "source", key, "duration_ms", run.DurationMS,
			"fetched", run.Fetched, "error", run.Error)
	case model.StatusEmpty:
		s.log.Warn("crawl returned no items", "source", key, "duration_ms", run.DurationMS,
			"note", run.Note)
	default:
		s.log.Info("crawl finished", "source", key, "duration_ms", run.DurationMS,
			"fetched", run.Fetched, "inserted", run.Inserted,
			"updated", run.Updated, "unchanged", run.Unchanged)
	}
}

// Stop halts scheduling and waits for in-flight crawls, bounded by ctx.
func (s *Scheduler) Stop(ctx context.Context) {
	s.cancel()

	// cron.Stop returns a context that closes once running jobs return.
	cronDone := s.cron.Stop()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.log.Info("all crawls finished")
	case <-cronDone.Done():
	case <-ctx.Done():
		s.log.Warn("shutdown deadline reached; abandoning in-flight crawls")
	}
}

// RunNow crawls specific sources synchronously. An empty list means every
// enabled source. It is used by the `exams collect` subcommand.
func (s *Scheduler) RunNow(ctx context.Context, keys []string) ([]model.CrawlRun, error) {
	if len(keys) == 0 {
		sources, err := s.store.ListSources(ctx)
		if err != nil {
			return nil, err
		}
		for _, src := range sources {
			if !src.Enabled {
				continue
			}
			if _, ok := collector.Get(src.Key); !ok {
				continue
			}
			keys = append(keys, src.Key)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no enabled sources to crawl")
	}

	var runs []model.CrawlRun
	for _, key := range keys {
		release, ok, err := s.store.TryLock(ctx, key)
		if err != nil {
			return runs, err
		}
		if !ok {
			return runs, fmt.Errorf("source %q is already being crawled by another process", key)
		}
		run, err := s.runner.RunSource(ctx, key)
		release()
		if err != nil {
			return runs, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}
