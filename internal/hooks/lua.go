package hooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"

	lua "github.com/yuin/gopher-lua"
)

// runLua executes an inline Lua hook script and returns its result.
//
// The script runs in a sandboxed interpreter (no filesystem, network,
// process, eval, or require) so a hook carries the same trust as any
// other user config without exposing host I/O. The embedded interpreter
// is pure Go, so Lua hooks behave identically across platforms — unlike
// shell hooks, they do not depend on any external binaries.
//
// The payload is exposed to the script as a global table `hook` with
// fields: event, session_id, cwd, tool_name, and tool_input (a nested
// table). The script signals its decision by returning a table whose
// shape mirrors the JSON stdout envelope of shell hooks, e.g.:
//
//	return { decision = "deny", reason = "..." }
//	return { context = "extra info", updated_input = { limit = 100 } }
//
// Returning nil (or nothing) means "no opinion" (DecisionNone). The
// returned table is bridged through the same envelope parser used for
// shell hook stdout, so decision/halt/reason/context/updated_input
// semantics are identical between the two hook kinds.
func runLua(ctx context.Context, source string, payload []byte) HookResult {
	L := newLuaSandbox()
	defer L.Close()
	L.SetContext(ctx)

	if err := setHookGlobal(L, payload); err != nil {
		slog.Warn("Lua hook: failed to build input table; skipping", "error", err)
		return HookResult{Decision: DecisionNone}
	}

	fn, err := L.LoadString(source)
	if err != nil {
		// A parse error is a non-blocking authoring error, mirroring a
		// shell hook that exits with an unrecognized non-zero code.
		slog.Warn("Lua hook: parse error; ignoring", "error", err)
		return HookResult{Decision: DecisionNone}
	}

	L.Push(fn)
	if err := L.PCall(0, lua.MultRet, nil); err != nil {
		if ctx.Err() != nil {
			slog.Warn("Lua hook timed out or was cancelled", "error", ctx.Err())
		} else {
			slog.Warn("Lua hook: runtime error; ignoring", "error", err)
		}
		return HookResult{Decision: DecisionNone}
	}

	top := L.GetTop()
	if top == 0 {
		return HookResult{Decision: DecisionNone}
	}
	ret := L.Get(top)
	if ret == lua.LNil {
		return HookResult{Decision: DecisionNone}
	}

	// Bridge the returned value through the JSON envelope parser so all
	// decision/context/updated_input handling matches shell hooks exactly.
	envelope, err := json.Marshal(luaToGo(ret))
	if err != nil {
		slog.Warn("Lua hook: could not encode return value; ignoring", "error", err)
		return HookResult{Decision: DecisionNone}
	}
	return parseStdout(string(envelope))
}

// setHookGlobal decodes the stdin payload JSON and exposes it to the
// script as the global table `hook`.
func setHookGlobal(L *lua.LState, payload []byte) error {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return err
	}
	L.SetGlobal("hook", goToLua(L, m))
	return nil
}

// newLuaSandbox creates a *lua.LState with only safe base libraries
// loaded: no filesystem, network, process, eval, or require. Mirrors the
// workflow engine's sandbox; hooks need no host I/O — their entire
// contract is the `hook` input table and a returned decision table.
func newLuaSandbox() *lua.LState {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})

	lua.OpenBase(L)
	for _, name := range []string{
		"load", "loadstring", "loadfile", "dofile",
		"require", "module", "newproxy",
		"getfenv", "setfenv", "_printregs",
	} {
		L.SetGlobal(name, lua.LNil)
	}
	lua.OpenTable(L)
	lua.OpenString(L)
	lua.OpenMath(L)
	// Deliberately not opened: package/require, io, os, debug, coroutine.

	return L
}

// luaToGo converts a Lua value into a plain Go value (string, float64,
// bool, nil, []any, or map[string]any) suitable for JSON encoding. Lua
// tables are treated as arrays when they have a contiguous 1..N integer
// key sequence with no other keys, and as maps otherwise.
func luaToGo(v lua.LValue) any {
	switch v := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(v)
	case lua.LNumber:
		return float64(v)
	case lua.LString:
		return string(v)
	case *lua.LTable:
		return luaTableToGo(v)
	default:
		return nil
	}
}

func luaTableToGo(t *lua.LTable) any {
	n := t.Len()
	isArray := n > 0
	if isArray {
		extra := 0
		t.ForEach(func(k, _ lua.LValue) {
			if _, ok := k.(lua.LNumber); !ok {
				extra++
			}
		})
		if extra > 0 {
			isArray = false
		}
	}

	if isArray {
		arr := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			arr = append(arr, luaToGo(t.RawGetInt(i)))
		}
		return arr
	}

	m := make(map[string]any)
	t.ForEach(func(k, v lua.LValue) {
		m[lua.LVAsString(k)] = luaToGo(v)
	})
	return m
}

// goToLua converts a plain Go value (as produced by JSON unmarshaling)
// into a Lua value.
func goToLua(L *lua.LState, v any) lua.LValue {
	switch v := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(v)
	case string:
		return lua.LString(v)
	case float64:
		return lua.LNumber(v)
	case int:
		return lua.LNumber(v)
	case int64:
		return lua.LNumber(v)
	case []any:
		tbl := L.NewTable()
		for i, item := range v {
			tbl.RawSetInt(i+1, goToLua(L, item))
		}
		return tbl
	case map[string]any:
		tbl := L.NewTable()
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			tbl.RawSetString(k, goToLua(L, v[k]))
		}
		return tbl
	default:
		return lua.LNil
	}
}
