package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/BenLocal/exams/internal/collector"
	"github.com/BenLocal/exams/internal/model"
	"github.com/BenLocal/exams/internal/store"
	"github.com/cloudwego/hertz/pkg/app"
)

// ---------------------------------------------------------------------------
// Views
//
// Templates never receive raw model rows. A view struct keeps a future column
// (a raw body, an error string) from leaking onto the page, and gives the
// filter controls something to compute against.

type baseView struct {
	Title string
	Nav   string
	Stats model.Stats
}

type listView struct {
	baseView
	Facets      store.Facets
	Filter      model.ItemFilter
	Page        model.Page
	PageNumbers []int
}

// baseQuery rebuilds the current filter as query parameters.
func (v listView) baseQuery() url.Values {
	q := url.Values{}
	if v.Filter.Query != "" {
		q.Set("q", v.Filter.Query)
	}
	if v.Filter.Source != "" {
		q.Set("source", v.Filter.Source)
	}
	if v.Filter.Category != "" {
		q.Set("category", v.Filter.Category)
	}
	if v.Filter.Region != "" {
		q.Set("region", v.Filter.Region)
	}
	if v.Filter.Unread {
		q.Set("unread", "1")
	}
	if v.Filter.PerPage != 20 {
		q.Set("per_page", strconv.Itoa(v.Filter.PerPage))
	}
	return q
}

// PageURL is the link to a given results page under the current filter.
func (v listView) PageURL(page int) string {
	q := v.baseQuery()
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// ToggleURL flips one filter value on or off.
func (v listView) ToggleURL(key, value string) string {
	q := v.baseQuery()
	if q.Get(key) == value {
		q.Del(key)
	} else {
		q.Set(key, value)
	}
	// Any filter change invalidates the current page number.
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// IsActive reports whether a facet value is currently applied.
func (v listView) IsActive(key, value string) bool {
	switch key {
	case "source":
		return v.Filter.Source == value
	case "category":
		return v.Filter.Category == value
	case "region":
		return v.Filter.Region == value
	case "unread":
		return v.Filter.Unread
	}
	return false
}

// HasFilters reports whether any filter is applied, so the page can offer a
// clear-all link.
func (v listView) HasFilters() bool {
	return v.Filter.Query != "" || v.Filter.Source != "" ||
		v.Filter.Category != "" || v.Filter.Region != "" || v.Filter.Unread
}

type detailView struct {
	baseView
	Exam    model.Exam
	Changes []model.ExamChange
}

// sourceView pairs a stored source with whether this binary still has a
// collector for it.
//
// Registered is derived state and deliberately does not live on model.Source:
// the store never populates it, and a field that is silently always false is
// an easy thing for a scheduler to trust by mistake.
type sourceView struct {
	model.Source
	Registered bool
}

type runsView struct {
	baseView
	Runs     []model.CrawlRun
	Sources  []sourceView
	NotifOK  int
	NotifBad int
}

type sourcesView struct {
	baseView
	Sources []sourceView
}

// ---------------------------------------------------------------------------
// Handlers

func (s *Server) handleIndex(ctx context.Context, c *app.RequestContext) {
	filter := filterFromQuery(c)

	page, err := s.store.ListExams(ctx, filter)
	if err != nil {
		s.fail(c, "list exams failed", err)
		return
	}
	stats, err := s.store.Stats(ctx)
	if err != nil {
		s.fail(c, "stats failed", err)
		return
	}
	facets, err := s.store.Facets(ctx)
	if err != nil {
		s.fail(c, "facets failed", err)
		return
	}

	s.render(c, 200, "index.html", listView{
		baseView:    baseView{Title: "考试信息", Nav: "index", Stats: stats},
		Facets:      facets,
		Filter:      filter,
		Page:        page,
		PageNumbers: pageWindow(page.Page, page.TotalPages, 2),
	})
}

func (s *Server) handleDetail(ctx context.Context, c *app.RequestContext) {
	id, ok := parseID(c.Param("id"))
	if !ok {
		c.String(400, "无效的考试 ID")
		return
	}

	exam, err := s.store.GetExam(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			c.String(404, "考试信息不存在")
			return
		}
		s.fail(c, "get exam failed", err)
		return
	}
	changes, err := s.store.ExamChanges(ctx, id, 50)
	if err != nil {
		s.fail(c, "exam changes failed", err)
		return
	}
	stats, err := s.store.Stats(ctx)
	if err != nil {
		s.fail(c, "stats failed", err)
		return
	}

	s.render(c, 200, "detail.html", detailView{
		baseView: baseView{Title: exam.Title, Nav: "index", Stats: stats},
		Exam:     exam,
		Changes:  changes,
	})
}

func (s *Server) handleMarkRead(ctx context.Context, c *app.RequestContext) {
	id, ok := parseID(c.Param("id"))
	if !ok {
		c.String(400, "无效的考试 ID")
		return
	}
	read := c.PostForm("read") != "0"

	if err := s.store.MarkRead(ctx, id, read); err != nil {
		if err == store.ErrNotFound {
			c.String(404, "考试信息不存在")
			return
		}
		s.fail(c, "mark read failed", err)
		return
	}
	c.Redirect(303, []byte(safeNext(c.PostForm("next"), "/exams/"+c.Param("id"))))
}

func (s *Server) handleMarkAllRead(ctx context.Context, c *app.RequestContext) {
	if _, err := s.store.MarkAllRead(ctx); err != nil {
		s.fail(c, "mark all read failed", err)
		return
	}
	c.Redirect(303, []byte(safeNext(c.PostForm("next"), "/")))
}

func (s *Server) handleRuns(ctx context.Context, c *app.RequestContext) {
	runs, err := s.store.ListRuns(ctx, c.Query("source"), 100)
	if err != nil {
		s.fail(c, "list runs failed", err)
		return
	}
	sources, err := s.loadSources(ctx)
	if err != nil {
		s.fail(c, "list sources failed", err)
		return
	}
	stats, err := s.store.Stats(ctx)
	if err != nil {
		s.fail(c, "stats failed", err)
		return
	}
	sent, failed, err := s.store.NotificationCounts(ctx)
	if err != nil {
		s.fail(c, "notification counts failed", err)
		return
	}

	s.render(c, 200, "runs.html", runsView{
		baseView: baseView{Title: "抓取任务", Nav: "runs", Stats: stats},
		Runs:     runs, Sources: sources,
		NotifOK: sent, NotifBad: failed,
	})
}

// handleTrigger enqueues crawls and returns immediately.
//
// It must never crawl inline: a crawl takes far longer than an HTTP request
// should, and Hertz's default request timeout would cut it off mid-run.
func (s *Server) handleTrigger(ctx context.Context, c *app.RequestContext) {
	key := strings.TrimSpace(c.PostForm("source"))

	queued := 0
	if key != "" && key != "all" {
		if s.sched.Trigger(key) {
			queued = 1
		}
	} else {
		// loadSources, not store.ListSources: only it knows which rows still
		// have a collector in this binary.
		sources, err := s.loadSources(ctx)
		if err != nil {
			s.fail(c, "list sources failed", err)
			return
		}
		for _, src := range sources {
			if src.Enabled && src.Registered && s.sched.Trigger(src.Key) {
				queued++
			}
		}
	}
	s.log.Info("manual crawl requested", "source", key, "queued", queued)

	c.Redirect(303, []byte(safeNext(c.PostForm("next"), "/runs")))
}

func (s *Server) handleSources(ctx context.Context, c *app.RequestContext) {
	sources, err := s.loadSources(ctx)
	if err != nil {
		s.fail(c, "list sources failed", err)
		return
	}
	stats, err := s.store.Stats(ctx)
	if err != nil {
		s.fail(c, "stats failed", err)
		return
	}

	s.render(c, 200, "sources.html", sourcesView{
		baseView: baseView{Title: "数据源", Nav: "sources", Stats: stats},
		Sources:  sources,
	})
}

func (s *Server) handleToggleSource(ctx context.Context, c *app.RequestContext) {
	key := c.Param("key")
	if key == "" {
		c.String(400, "缺少数据源标识")
		return
	}
	src, err := s.store.GetSource(ctx, key)
	if err != nil {
		c.String(404, "数据源不存在")
		return
	}
	if err := s.store.SetSourceEnabled(ctx, key, !src.Enabled); err != nil {
		s.fail(c, "toggle source failed", err)
		return
	}
	c.Redirect(303, []byte("/sources"))
}

func (s *Server) handleHealth(ctx context.Context, c *app.RequestContext) {
	if err := s.store.Ping(ctx); err != nil {
		c.JSON(503, map[string]string{"status": "unhealthy", "error": err.Error()})
		return
	}
	c.JSON(200, map[string]string{"status": "ok"})
}

// loadSources annotates database rows with whether a matching collector still
// exists in the binary. An orphan row is shown but never scheduled.
func (s *Server) loadSources(ctx context.Context) ([]sourceView, error) {
	sources, err := s.store.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sourceView, 0, len(sources))
	for _, src := range sources {
		_, registered := collector.Get(src.Key)
		out = append(out, sourceView{Source: src, Registered: registered})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers

// pageWindow returns the page numbers to offer around the current page.
func pageWindow(current, total, span int) []int {
	if total <= 1 {
		return nil
	}
	start := current - span
	if start < 1 {
		start = 1
	}
	end := start + 2*span
	if end > total {
		end = total
		start = end - 2*span
		if start < 1 {
			start = 1
		}
	}
	out := make([]int, 0, end-start+1)
	for i := start; i <= end; i++ {
		out = append(out, i)
	}
	return out
}

// safeNext validates a post-action redirect target.
//
// Only same-site absolute paths are accepted: "//evil.example" is a
// protocol-relative URL that would send the browser off-site.
func safeNext(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return raw
	}
	return fallback
}
