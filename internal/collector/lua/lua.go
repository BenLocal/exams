// Package lua runs crawlers written as Lua scripts.
//
// A script implements the same contract as a Go collector, against the same
// Fetcher — so retries, per-host pacing, GBK decoding and charset sniffing come
// for free and a script author never has to think about them.
//
// Scripts live in a directory and are loaded at startup, which is the point:
// adding a provincial exam board should not require recompiling the binary.
//
// A Lua script is arbitrary code. The interpreter is opened without os, io,
// package or debug, and dofile/loadfile are removed, but that is accident
// prevention rather than a security boundary — only run scripts you wrote.
package lua

import (
	"context"
	"fmt"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// stateInitTimeout bounds executing a script to read its metadata at load
// time. It should only build a table, so anything slower is a mistake worth
// reporting rather than waiting on.
const stateInitTimeout = 5 // seconds

// newState builds a sandboxed interpreter.
func newState(ctx context.Context) *lua.LState {
	if ctx == nil {
		// SetContext panics on a nil context; load-time work has no run to
		// belong to.
		ctx = context.Background()
	}
	L := lua.NewState(lua.Options{
		SkipOpenLibs: true,
		// Modest limits: these scripts parse a page, they do not compute.
		CallStackSize: 120,
		RegistrySize:  1024 * 20,
	})

	// Opened selectively. os, io, package and debug are never opened: a
	// collector has no business reading files, spawning processes or loading
	// further code.
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		// Each Open* registers its own global through RegisterModule, so the
		// value it leaves on the stack is not needed. NRet: 0 discards it;
		// reading and popping instead would underflow the stack, because
		// CallByParam has already consumed the function and arguments.
		if err := L.CallByParam(lua.P{
			Fn:      L.NewFunction(lib.fn),
			NRet:    0,
			Protect: true,
		}); err != nil {
			panic(fmt.Sprintf("lua: opening %s: %v", lib.name, err))
		}
	}

	// The base library is not as harmless as it looks. It registers dofile and
	// loadfile, which read from the file system, and require, which loads
	// further code — even though the package library that backs require is
	// never opened. Removing all three keeps the "no filesystem, no further
	// code loading" claim honest.
	//
	// load and loadstring stay: they only compile strings in memory, and a
	// script already contains arbitrary code.
	for _, name := range []string{"dofile", "loadfile", "require"} {
		L.SetGlobal(name, lua.LNil)
	}

	// A cancelled or timed-out crawl aborts the script mid-loop.
	L.SetContext(ctx)

	registerSelectionType(L)
	registerContextType(L)

	return L
}

// loadChunk compiles a script without running it.
func loadChunk(L *lua.LState, name, src string) (*lua.FunctionProto, error) {
	chunk, err := L.Load(strings.NewReader(src), name)
	if err != nil {
		return nil, err
	}
	if chunk.Proto == nil {
		// Only a Go function has no prototype, and Load never returns one.
		return nil, fmt.Errorf("lua: %s: compiled chunk has no prototype", name)
	}
	return chunk.Proto, nil
}

// luaError converts a Lua error into a Go error carrying the script name.
//
// gopher-lua returns errors as *lua.ApiError with a stack trace; keeping that
// trace is what makes a failing callback inside a script debuggable at all.
func luaError(scriptName string, err error) error {
	if err == nil {
		return nil
	}
	if apiErr, ok := err.(*lua.ApiError); ok {
		return fmt.Errorf("%s: %s", scriptName, apiErr.Error())
	}
	return fmt.Errorf("%s: %w", scriptName, err)
}

// stringField reads an optional string field from a table.
func stringField(L *lua.LState, tbl *lua.LTable, key string) string {
	v := L.GetField(tbl, key)
	if v == lua.LNil {
		return ""
	}
	if s, ok := v.(lua.LString); ok {
		return strings.TrimSpace(string(s))
	}
	return ""
}

// funcField returns a function field, or nil when absent or not a function.
func funcField(L *lua.LState, tbl *lua.LTable, key string) *lua.LFunction {
	v := L.GetField(tbl, key)
	if fn, ok := v.(*lua.LFunction); ok {
		return fn
	}
	return nil
}
