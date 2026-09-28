package lua

import (
	"fmt"

	"github.com/BenLocal/exams/internal/collector"
	"github.com/BenLocal/exams/internal/model"
	"github.com/PuerkitoBio/goquery"
	lua "github.com/yuin/gopher-lua"
)

const selectionTypeName = "exams.selection"

// selection is the Lua-facing wrapper around a goquery match set.
type selection struct {
	sel *goquery.Selection
}

func registerSelectionType(L *lua.LState) {
	mt := L.NewTypeMetatable(selectionTypeName)
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), selectionMethods))
	// Without this a selection prints as "userdata: 0x…" in error messages.
	L.SetField(mt, "__tostring", L.NewFunction(selectionToString))
	L.SetField(mt, "__len", L.NewFunction(selLen))
}

// wrapSelection returns a Lua value wrapping s.
func wrapSelection(L *lua.LState, s *goquery.Selection) lua.LValue {
	ud := L.NewUserData()
	ud.Value = &selection{sel: s}
	L.SetMetatable(ud, L.GetTypeMetatable(selectionTypeName))
	return ud
}

func checkSelection(L *lua.LState, n int) *goquery.Selection {
	ud := L.CheckUserData(n)
	if v, ok := ud.Value.(*selection); ok && v.sel != nil {
		return v.sel
	}
	L.ArgError(n, "selection expected")
	return nil
}

var selectionMethods = map[string]lua.LGFunction{
	"find":         selFind,
	"first":        selFirst,
	"children":     selChildren,
	"parent":       selParent,
	"text":         selText,
	"body":         selBody,
	"html":         selHTML,
	"attr":         selAttr,
	"has":          selHas,
	"len":          selLen,
	"each":         selEach,
	"each_checked": selEachChecked,
}

func selFind(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(wrapSelection(L, sel.Find(L.CheckString(2))))
	return 1
}

func selFirst(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(wrapSelection(L, sel.First()))
	return 1
}

func selChildren(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(wrapSelection(L, sel.Children()))
	return 1
}

func selParent(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(wrapSelection(L, sel.Parent()))
	return 1
}

// selText returns the node's text with whitespace collapsed, matching what the
// Go collectors store.
func selText(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(lua.LString(model.NormalizeText(sel.Text())))
	return 1
}

// selBody is like text() but keeps paragraph breaks, so a long announcement
// does not collapse into a single wall of text. Use it for article bodies;
// text() is the right choice for titles and dates.
func selBody(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(lua.LString(collector.TextWithBreaks(sel)))
	return 1
}

func selHTML(L *lua.LState) int {
	sel := checkSelection(L, 1)
	html, err := sel.Html()
	if err != nil {
		L.RaiseError("html(): %v", err)
		return 0
	}
	L.Push(lua.LString(html))
	return 1
}

// selAttr returns the attribute, or an empty string when it is absent.
//
// Returning "" rather than nil keeps scripts free of nil checks in the common
// `a:attr("title") or a:text()` idiom without changing its meaning.
func selAttr(L *lua.LState) int {
	sel := checkSelection(L, 1)
	v, _ := sel.Attr(L.CheckString(2))
	L.Push(lua.LString(v))
	return 1
}

func selHas(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(lua.LBool(sel.Find(L.CheckString(2)).Length() > 0))
	return 1
}

func selLen(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(lua.LNumber(sel.Length()))
	return 1
}

// selEach calls fn once per match and collects the non-nil results into an
// array. This is the workhorse of a list parser:
//
//	return doc:each("ul li", function(row)
//	  local a = row:find("a")
//	  return { title = a:text(), url = a:attr("href") }
//	end)
func selEach(L *lua.LState) int {
	return eachImpl(L, false)
}

// selEachChecked is selEach plus the guard that matters most in practice.
//
// When a selector stops matching because the site was restyled, an unguarded
// script returns an empty list, the crawl is recorded as "succeeded", and the
// data silently stops arriving. Raising here names the selector instead.
func selEachChecked(L *lua.LState) int {
	return eachImpl(L, true)
}

func eachImpl(L *lua.LState, checked bool) int {
	sel := checkSelection(L, 1)
	selector := L.CheckString(2)
	fn := L.CheckFunction(3)

	// An index loop rather than goquery's Each: Each cannot be aborted, and a
	// failure inside the callback has to stop the run rather than be repeated
	// for every remaining row.
	matches := sel.Find(selector)
	n := matches.Length()
	if checked && n == 0 {
		L.RaiseError("selector %q matched 0 nodes; the site markup probably changed", selector)
		return 0
	}

	out := L.NewTable()
	idx := 1
	for i := 0; i < n; i++ {
		row := matches.Eq(i)
		if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true},
			wrapSelection(L, row)); err != nil {
			// Re-raise so the caller sees the script's own stack trace.
			L.RaiseError("%s", err.Error())
			return 0
		}
		ret := L.Get(-1)
		L.Pop(1)
		if ret == lua.LNil {
			// A callback returning nothing means "skip this row" — a
			// convenient way to filter inside the loop.
			continue
		}
		out.RawSetInt(idx, ret)
		idx++
	}

	L.Push(out)
	return 1
}

// selectionToString makes a selection readable in error messages and in
// Lua's tostring(), rather than the default "userdata: 0x…".
func selectionToString(L *lua.LState) int {
	sel := checkSelection(L, 1)
	L.Push(lua.LString(fmt.Sprintf("<selection %d node(s)>", sel.Length())))
	return 1
}
