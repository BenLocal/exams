// Package web serves the HTML pages and the JSON API.
//
// Templates and CSS are embedded in the binary and the stylesheet is inlined
// in the layout, so the app has no static-file route, no CDN dependency and
// works with no network at all.
package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/BenLocal/exams/internal/model"
	"github.com/BenLocal/exams/internal/scheduler"
	"github.com/BenLocal/exams/internal/store"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
)

//go:embed templates/*.html
var templateFS embed.FS

// Every page is parsed together with the partials as its own template set.
//
// Parsing all pages into one set would make their shared {{define}} blocks
// collide, with the last file parsed silently winning.
var pageNames = []string{"index.html", "detail.html", "runs.html", "sources.html"}

// Server renders pages and serves the JSON API.
type Server struct {
	store *store.Store
	sched *scheduler.Scheduler
	log   *slog.Logger
	tmpl  map[string]*template.Template
}

// New builds the web layer.
func New(st *store.Store, sched *scheduler.Scheduler, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{store: st, sched: sched, log: log, tmpl: tmpl}, nil
}

func parseTemplates() (map[string]*template.Template, error) {
	out := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(funcMap).
			ParseFS(templateFS, "templates/partials.html", "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

// Register wires the routes onto a Hertz engine.
func (s *Server) Register(h *server.Hertz) {
	h.Use(s.sameOriginGuard)

	h.GET("/", s.handleIndex)
	h.GET("/exams/:id", s.handleDetail)
	h.POST("/exams/:id/read", s.handleMarkRead)
	// Deliberately not /exams/read-all: a literal and a wildcard segment
	// cannot coexist as siblings in this router.
	h.POST("/read-all", s.handleMarkAllRead)

	h.GET("/runs", s.handleRuns)
	h.POST("/runs/trigger", s.handleTrigger)
	h.GET("/sources", s.handleSources)
	h.POST("/sources/:key/toggle", s.handleToggleSource)

	h.GET("/api/exams", s.apiListExams)
	h.GET("/api/exams/:id", s.apiGetExam)
	h.GET("/api/runs", s.apiRuns)
	h.GET("/api/stats", s.apiStats)
	h.GET("/healthz", s.handleHealth)
}

// render executes a page into a buffer before writing anything.
//
// Rendering straight into the response would send a 200 and a partial page
// before a template error surfaced, leaving the browser with truncated HTML
// and no sign that anything failed.
func (s *Server) render(c *app.RequestContext, status int, name string, data any) {
	t, ok := s.tmpl[name]
	if !ok {
		s.log.Error("unknown template", "name", name)
		c.String(500, "template %s not found", name)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("template render failed", "name", name, "err", err)
		c.String(500, "页面渲染失败")
		return
	}
	// Without this the browser will serve a stale unread count after a
	// POST-and-redirect.
	c.Header("Cache-Control", "no-store")
	c.Data(status, "text/html; charset=utf-8", buf.Bytes())
}

// fail logs an error and returns a minimal 500 page.
func (s *Server) fail(c *app.RequestContext, msg string, err error) {
	s.log.Error(msg, "err", err, "path", string(c.Path()))
	c.Header("Cache-Control", "no-store")
	c.String(500, "服务器内部错误，请查看服务日志。")
}

// sameOriginGuard rejects cross-origin state-changing requests.
//
// The app has no authentication by design, so the realistic threat is a
// malicious page in the operator's browser silently POSTing to a local
// instance. Browsers always attach Origin to such requests, which makes this
// check sufficient. Non-browser clients send neither header and are allowed
// through, which keeps curl usable.
func (s *Server) sameOriginGuard(ctx context.Context, c *app.RequestContext) {
	switch string(c.Method()) {
	case "GET", "HEAD", "OPTIONS":
		c.Next(ctx)
		return
	}

	origin := string(c.GetHeader("Origin"))
	if origin == "" {
		origin = string(c.GetHeader("Referer"))
	}
	if origin == "" {
		c.Next(ctx)
		return
	}

	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Host, string(c.Host())) {
		s.log.Warn("rejected cross-origin request",
			"origin", origin, "host", string(c.Host()), "path", string(c.Path()))
		c.AbortWithStatus(403)
		return
	}
	c.Next(ctx)
}

// ---------------------------------------------------------------------------
// Template helpers

var funcMap = template.FuncMap{
	"fmtTime": func(t *time.Time) string { return model.FormatTime(t) },
	// fmtDate renders the date alone. Announcements are filed by date, and the
	// time is always the 09:00 anchor the parser applies, so showing it would
	// be noise dressed as precision.
	"fmtDate": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "—"
		}
		return t.Format("2006-01-02")
	},
	"since": since,
	"dash": func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "—"
		}
		return s
	},
	"hasPrefix": strings.HasPrefix,
	"add":       func(a, b int) int { return a + b },
	"sub":       func(a, b int) int { return a - b },
	"join": func(parts []string) string {
		if len(parts) == 0 {
			return "—"
		}
		return strings.Join(parts, "、")
	},
	// paragraphs splits stored body text so each paragraph can carry its own
	// two-character first-line indent. Applying the indent to the whole block
	// at once would only indent the first line of the entire announcement.
	"paragraphs": func(body string) []string {
		var out []string
		for _, block := range strings.Split(body, "\n\n") {
			// A single newline inside a block is a soft wrap, not a break.
			p := strings.TrimSpace(strings.ReplaceAll(block, "\n", " "))
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	},
	"fieldLabels": func(fields []string) []string {
		out := make([]string, 0, len(fields))
		for _, f := range fields {
			out = append(out, fieldLabel(f))
		}
		return out
	},
	// ptr lets fmtTime handle non-pointer times from tables without every
	// call site needing its own nullable wrapper.
	"ptr": func(t time.Time) *time.Time { return &t },
	"statusLabel": func(status string) string {
		switch status {
		case model.StatusSuccess:
			return "成功"
		case model.StatusFailed:
			return "失败"
		case model.StatusEmpty:
			return "抓到 0 条"
		case model.StatusRunning:
			return "进行中"
		}
		return status
	},
	// statusClass drives the ledger's text marks, not a pill badge.
	"statusClass": func(status string) string {
		switch status {
		case model.StatusSuccess:
			return "mark-ok"
		case model.StatusFailed:
			return "mark-bad"
		case model.StatusEmpty:
			return "mark-warn"
		}
		return "mark-idle"
	},
	"fieldLabel": fieldLabel,
}

// since renders how long ago something happened, in the register a person
// would use when glancing at the board. A nil time means the thing has never
// happened, which is worth saying plainly rather than rendering as a date.
func since(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "尚未抓取"
	}
	d := time.Since(*t)
	switch {
	case d < 0:
		// Clock skew between the crawler's host and the reader's, or a
		// timestamp a moment in the future.
		return "刚刚"
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// fieldLabel renders a changed-field name in Chinese for the change history.
func fieldLabel(field string) string {
	switch field {
	case "title":
		return "标题"
	case "url":
		return "链接"
	case "summary":
		return "摘要"
	case "content":
		return "正文"
	case "category":
		return "分类"
	case "region":
		return "地区"
	case "published_at", "published_raw":
		return "发布日期"
	case "deadline_at":
		return "报名截止"
	default:
		return field
	}
}

// ---------------------------------------------------------------------------
// Query parsing shared by the pages and the JSON API

func filterFromQuery(c *app.RequestContext) model.ItemFilter {
	return model.ItemFilter{
		Query:    strings.TrimSpace(c.Query("q")),
		Source:   c.Query("source"),
		Category: c.Query("category"),
		Region:   c.Query("region"),
		Unread:   c.Query("unread") == "1",
		Page:     intParam(c, "page", 1),
		PerPage:  clampInt(intParam(c, "per_page", 20), 1, 100),
	}
}

func intParam(c *app.RequestContext, key string, def int) int {
	raw := c.Query(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func parseID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
