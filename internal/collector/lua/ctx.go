package lua

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/BenLocal/exams/internal/collector"
	lua "github.com/yuin/gopher-lua"
)

const contextTypeName = "exams.ctx"

// scriptCtx is what a script receives as its first argument. It bundles the
// per-run state so the script never has to know about HTTP concerns.
type scriptCtx struct {
	ctx     context.Context
	fetcher *collector.Fetcher
	base    string
	loc     *time.Location
	log     *slog.Logger
	key     string
}

func registerContextType(L *lua.LState) {
	mt := L.NewTypeMetatable(contextTypeName)
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), contextMethods))
	L.SetField(mt, "__tostring", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString("<exams context>"))
		return 1
	}))
}

func pushContext(L *lua.LState, c *scriptCtx) {
	ud := L.NewUserData()
	ud.Value = c
	L.SetMetatable(ud, L.GetTypeMetatable(contextTypeName))
	L.Push(ud)
}

func checkContext(L *lua.LState, n int) *scriptCtx {
	ud := L.CheckUserData(n)
	if v, ok := ud.Value.(*scriptCtx); ok {
		return v
	}
	L.ArgError(n, "context expected")
	return nil
}

var contextMethods = map[string]lua.LGFunction{
	"get":        ctxGet,
	"get_text":   ctxGetText,
	"abs":        ctxAbs,
	"parse_date": ctxParseDate,
	"fail":       ctxFail,
	"log":        ctxLog,
}

// resolve turns a possibly-relative URL into an absolute one.
//
// Tracking parameters are deliberately NOT stripped here: a script may need
// them to reach the right page. Item URLs are canonicalised separately when
// the results are converted.
func (c *scriptCtx) resolve(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty URL")
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse URL %q: %w", raw, err)
	}
	if ref.IsAbs() {
		return ref.String(), nil
	}
	base, err := url.Parse(c.base)
	if err != nil {
		return "", fmt.Errorf("parse base URL %q: %w", c.base, err)
	}
	return base.ResolveReference(ref).String(), nil
}

// ctxGet fetches a page and returns it as a selection.
func ctxGet(L *lua.LState) int {
	c := checkContext(L, 1)
	target, err := c.resolve(L.CheckString(2))
	if err != nil {
		L.RaiseError("%v", err)
		return 0
	}

	doc, err := c.fetcher.Get(c.ctx, target)
	if err != nil {
		// The Fetcher's error already names the URL and the HTTP status.
		L.RaiseError("get %s: %v", target, err)
		return 0
	}
	// goquery.Document embeds *Selection, so the document and any node share
	// one Lua type.
	L.Push(wrapSelection(L, doc.Selection))
	return 1
}

// ctxGetText fetches a page and returns its decoded body as a string.
func ctxGetText(L *lua.LState) int {
	c := checkContext(L, 1)
	target, err := c.resolve(L.CheckString(2))
	if err != nil {
		L.RaiseError("%v", err)
		return 0
	}
	body, err := c.fetcher.GetBytes(c.ctx, target)
	if err != nil {
		L.RaiseError("get_text %s: %v", target, err)
		return 0
	}
	L.Push(lua.LString(string(body)))
	return 1
}

func ctxAbs(L *lua.LState) int {
	c := checkContext(L, 1)
	target, err := c.resolve(L.CheckString(2))
	if err != nil {
		// abs() is a convenience; an unresolvable href yields "" so a script
		// can filter with `if url == "" then return end`.
		L.Push(lua.LString(""))
		return 1
	}
	L.Push(lua.LString(target))
	return 1
}

// ctxParseDate turns a printed Chinese date into "2006-01-02", or nil.
//
// Scripts normally do not need this: an item's published/deadline fields are
// parsed on the Go side using the same rules as the built-in collectors.
func ctxParseDate(L *lua.LState) int {
	c := checkContext(L, 1)
	when, _ := collector.ParseDate(L.CheckString(2), c.loc)
	if when == nil {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(lua.LString(when.Format("2006-01-02")))
	return 1
}

// ctxFail aborts the script with a message.
//
// Intended for the guard every list parser should have: when a selector stops
// matching, fail loudly rather than returning an empty list that looks like a
// quiet news day.
func ctxFail(L *lua.LState) int {
	checkContext(L, 1)
	L.RaiseError("%s", L.CheckString(2))
	return 0
}

func ctxLog(L *lua.LState) int {
	c := checkContext(L, 1)
	c.log.Info("lua script", "source", c.key, "msg", L.CheckString(2))
	return 0
}

// ---------------------------------------------------------------------------
// Result conversion

// parseAnyDate accepts either a timestamp with an explicit zone, or the raw
// text printed on the page.
//
// RFC 3339 is tried first because it is the only form that carries its own
// zone. Everything else goes through collector.ParseDate — the same function
// the built-in collectors use — deliberately: parsing "2026-09-04" with
// time.ParseInLocation would give midnight, while ParseDate anchors at 09:00
// local, and a Lua source whose dates sat eight hours off from every Go source
// would be a confusing thing to debug.
func parseAnyDate(s string, loc *time.Location) (*time.Time, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, s
	}
	return collector.ParseDate(s, loc)
}

// tableToItem converts a Lua table returned by a script into a model.Item,
// starting from base so that a detail() implementation only needs to return
// the body.
func tableToItem(L *lua.LState, tbl *lua.LTable, base string, loc *time.Location) (itemFields, error) {
	out := itemFields{
		title:       stringField(L, tbl, "title"),
		url:         stringField(L, tbl, "url"),
		summary:     stringField(L, tbl, "summary"),
		content:     stringField(L, tbl, "content"),
		category:    stringField(L, tbl, "category"),
		region:      stringField(L, tbl, "region"),
		externalID:  stringField(L, tbl, "external_id"),
		publishedIn: firstNonEmpty(stringField(L, tbl, "published"), stringField(L, tbl, "published_at")),
		deadlineIn:  firstNonEmpty(stringField(L, tbl, "deadline"), stringField(L, tbl, "deadline_at")),
	}

	out.publishedRaw = stringField(L, tbl, "published_raw")
	publishedAt, parsedRaw := parseAnyDate(out.publishedIn, loc)
	out.publishedAt = publishedAt
	if out.publishedRaw == "" {
		// Preserve whatever the source printed, so an ambiguous parse never
		// destroys the original date text.
		out.publishedRaw = parsedRaw
	}
	out.deadlineAt, _ = parseAnyDate(out.deadlineIn, loc)

	// Resolve the link against the site root so that scripts can return the
	// href exactly as written in the markup.
	if out.url != "" {
		if abs, err := collector.ResolveURL(base, out.url); err == nil {
			out.url = abs
		}
	}
	if out.externalID == "" {
		// Default identity is the canonical detail URL, matching the built-in
		// collectors.
		out.externalID = out.url
	}
	return out, nil
}

// itemFields mirrors the writable subset of model.Item.
type itemFields struct {
	externalID   string
	title        string
	url          string
	summary      string
	content      string
	category     string
	region       string
	publishedAt  *time.Time
	publishedIn  string
	publishedRaw string
	deadlineAt   *time.Time
	deadlineIn   string
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
