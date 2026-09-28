package lua

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/BenLocal/exams/internal/collector"
	"github.com/BenLocal/exams/internal/model"
	lua "github.com/yuin/gopher-lua"
)

// Source is a collector backed by a Lua script. It implements
// collector.Collector, and collector.Detailer when the script defines a
// detail function.
type Source struct {
	path string

	key     string
	name    string
	baseURL string

	// Defaults applied to every item the script returns, so a script that
	// covers a single province does not have to repeat itself per row.
	category string
	region   string

	proto     *lua.FunctionProto
	hasDetail bool

	loc *time.Location
	log *slog.Logger
}

var (
	_ collector.Collector = (*Source)(nil)
	_ collector.Detailer  = (*Source)(nil)
)

func (s *Source) Key() string     { return s.key }
func (s *Source) Name() string    { return s.name }
func (s *Source) BaseURL() string { return s.baseURL }

// List runs the script's list function.
func (s *Source) List(ctx context.Context, f *collector.Fetcher) ([]model.Item, error) {
	L, src, err := s.exec(ctx, f)
	if err != nil {
		return nil, err
	}
	defer L.Close()

	fn := funcField(L, src, "list")
	if fn == nil {
		return nil, fmt.Errorf("%s: script does not define a list function", s.key)
	}

	sc := s.scriptCtx(ctx, f)
	if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, luaCtx(L, sc)); err != nil {
		return nil, luaError(s.key, err)
	}
	ret := L.Get(-1)
	L.Pop(1)

	// LNil is a value, not a type, so it is checked before the type switch.
	if ret == lua.LNil {
		return nil, nil
	}
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("%s: list must return a table of items, got %s",
			s.key, ret.Type())
	}
	return s.itemsFromTable(L, tbl)
}

// Detail runs the script's detail function, if it has one.
func (s *Source) Detail(ctx context.Context, f *collector.Fetcher, it model.Item) (model.Item, error) {
	if !s.hasDetail {
		return it, nil
	}

	L, src, err := s.exec(ctx, f)
	if err != nil {
		return it, err
	}
	defer L.Close()

	fn := funcField(L, src, "detail")
	if fn == nil {
		return it, nil
	}

	sc := s.scriptCtx(ctx, f)
	arg := itemToTable(L, it)

	if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, luaCtx(L, sc), arg); err != nil {
		return it, luaError(s.key, err)
	}
	ret := L.Get(-1)
	L.Pop(1)

	switch v := ret.(type) {
	case lua.LString:
		// The common shape: return just the body.
		it.Content = model.NormalizeBody(string(v))
		return it, nil
	case *lua.LTable:
		fields, err := tableToItem(L, v, s.baseURL, s.loc)
		if err != nil {
			return it, err
		}
		// Only overwrite what the script actually returned, so detail() can
		// return just the body.
		if fields.content != "" {
			it.Content = fields.content
		}
		if fields.title != "" {
			it.Title = fields.title
		}
		if fields.url != "" {
			it.URL = fields.url
		}
		if fields.summary != "" {
			it.Summary = fields.summary
		}
		if fields.deadlineAt != nil {
			it.DeadlineAt = fields.deadlineAt
		}
		return it, nil
	}
	if ret == lua.LNil {
		return it, nil
	}
	return it, fmt.Errorf("%s: detail must return a string or a table, got %s",
		s.key, ret.Type())
}

// exec builds a fresh interpreter and runs the script's top level, returning
// the source table it defines.
//
// A new interpreter per call is deliberate. Reusing one would let state leak
// between crawls, and would make concurrent runs of different sources share a
// VM that is not safe for concurrent use. Compilation is not repeated — the
// prototype is parsed once at load time.
func (s *Source) exec(ctx context.Context, f *collector.Fetcher) (*lua.LState, *lua.LTable, error) {
	L := newState(ctx)

	L.Push(L.NewFunctionFromProto(s.proto))
	if err := L.PCall(0, 1, nil); err != nil {
		L.Close()
		return nil, nil, luaError(s.key, err)
	}

	tbl, ok := L.Get(-1).(*lua.LTable)
	if !ok {
		L.Close()
		return nil, nil, fmt.Errorf("%s: script must return a table, got %s",
			s.key, L.Get(-1).Type())
	}
	L.Pop(1)

	return L, tbl, nil
}

func (s *Source) scriptCtx(ctx context.Context, f *collector.Fetcher) *scriptCtx {
	return &scriptCtx{
		ctx:     ctx,
		fetcher: f,
		base:    s.baseURL,
		loc:     s.loc,
		log:     s.log,
		key:     s.key,
	}
}

func luaCtx(L *lua.LState, c *scriptCtx) lua.LValue {
	ud := L.NewUserData()
	ud.Value = c
	L.SetMetatable(ud, L.GetTypeMetatable(contextTypeName))
	return ud
}

// itemsFromTable converts the script's return value into items.
//
// Unusable rows are skipped rather than failing the whole source — a single
// malformed entry should not cost the run — but if every row was unusable that
// is a script bug, and saying so beats reporting a successful empty crawl.
func (s *Source) itemsFromTable(L *lua.LState, tbl *lua.LTable) ([]model.Item, error) {
	n := tbl.Len()
	items := make([]model.Item, 0, n)
	skipped := 0
	var firstReason string

	for i := 1; i <= n; i++ {
		row, ok := tbl.RawGetInt(i).(*lua.LTable)
		if !ok {
			skipped++
			if firstReason == "" {
				firstReason = fmt.Sprintf("entry %d is a %s, not a table", i, tbl.RawGetInt(i).Type())
			}
			continue
		}
		fields, err := tableToItem(L, row, s.baseURL, s.loc)
		if err != nil {
			return nil, err
		}

		title := fields.title
		if title == "" {
			skipped++
			if firstReason == "" {
				firstReason = fmt.Sprintf("entry %d has no title", i)
			}
			continue
		}
		// A row with neither a link nor an explicit id has no stable identity,
		// so re-crawling would duplicate it.
		if fields.externalID == "" {
			skipped++
			if firstReason == "" {
				firstReason = fmt.Sprintf("entry %d has no url and no external_id, so it has no stable identity", i)
			}
			continue
		}

		items = append(items, s.toItem(fields))
	}

	if len(items) == 0 && skipped > 0 {
		return nil, fmt.Errorf(
			"%s: list returned %d entries but none were usable (%s)\n"+
				"  Check the fields the script sets: title and url (or external_id) are required",
			s.key, skipped, firstReason)
	}
	if skipped > 0 {
		s.log.Warn("lua script returned unusable entries",
			"source", s.key, "skipped", skipped, "kept", len(items), "reason", firstReason)
	}
	return items, nil
}

// toItem applies the source-level defaults.
func (s *Source) toItem(fields itemFields) model.Item {
	category, region := fields.category, fields.region
	if category == "" {
		category = s.category
	}
	if region == "" {
		region = s.region
	}
	return model.Item{
		ExternalID:   fields.externalID,
		Title:        model.NormalizeText(fields.title),
		URL:          fields.url,
		Summary:      model.NormalizeText(fields.summary),
		Content:      fields.content,
		Category:     category,
		Region:       region,
		PublishedAt:  fields.publishedAt,
		PublishedRaw: fields.publishedRaw,
		DeadlineAt:   fields.deadlineAt,
	}
}

// itemToTable exposes an item to a detail function.
func itemToTable(L *lua.LState, it model.Item) *lua.LTable {
	t := L.NewTable()
	t.RawSetString("external_id", lua.LString(it.ExternalID))
	t.RawSetString("title", lua.LString(it.Title))
	t.RawSetString("url", lua.LString(it.URL))
	t.RawSetString("summary", lua.LString(it.Summary))
	t.RawSetString("category", lua.LString(it.Category))
	t.RawSetString("region", lua.LString(it.Region))
	if it.PublishedAt != nil {
		t.RawSetString("published", lua.LString(it.PublishedAt.Format("2006-01-02")))
	}
	if it.DeadlineAt != nil {
		t.RawSetString("deadline", lua.LString(it.DeadlineAt.Format("2006-01-02")))
	}
	return t
}

// metaResult is what reading a script's metadata at load time produces.
type metaResult struct {
	key      string
	name     string
	baseURL  string
	category string
	region   string
}

// readMeta runs a script once to read its declared metadata.
//
// The script body should only build and return a table — no fetching at load
// time — so this is cheap, and it is bounded by a short timeout anyway.
func readMeta(L *lua.LState, proto *lua.FunctionProto, path string) (metaResult, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stateInitTimeout*time.Second)
	defer cancel()
	L.SetContext(ctx)

	L.SetTop(0)
	L.Push(L.NewFunctionFromProto(proto))
	if err := L.PCall(0, 1, nil); err != nil {
		return metaResult{}, false, luaError(path, err)
	}
	tbl, ok := L.Get(-1).(*lua.LTable)
	if !ok {
		return metaResult{}, false, fmt.Errorf(
			"%s: the script must return a table, got %s\n"+
				"  A collector script ends with `return source`", path, L.Get(-1).Type())
	}
	L.Pop(1)

	m := metaResult{
		key:      stringField(L, tbl, "key"),
		name:     stringField(L, tbl, "name"),
		baseURL:  stringField(L, tbl, "base_url"),
		category: stringField(L, tbl, "category"),
		region:   stringField(L, tbl, "region"),
	}
	_, hasDetail := L.GetField(tbl, "detail").(*lua.LFunction)
	return m, hasDetail, nil
}

// describe renders the source for `exams sources` and for log lines.
func (s *Source) String() string {
	return fmt.Sprintf("lua:%s(%s)", s.key, strings.TrimPrefix(s.path, "./"))
}
