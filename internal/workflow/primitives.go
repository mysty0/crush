package workflow

import (
	lua "github.com/yuin/gopher-lua"
)

// luaPrelude is Lua source injected before every workflow script. It
// implements pipeline() purely in terms of the Go-native parallel()
// primitive: each item gets its own zero-arg closure that runs
// mapFn(item) then, once that resolves, thenFn(result) — exactly
// mirroring parallel()'s "spawn N branches, collect in order" model,
// just with a two-step body per branch. No dedicated Go-side
// streaming primitive is needed since each branch already runs on its
// own coroutine and yields independently for each of its agent()
// calls.
//
// NOTE: every call to agent()/parallel()/pipeline() whose result
// needs to flow back out through a yield MUST NOT be written in Lua
// tail-call position ("return f(...)"). gopher-lua's tail-call
// optimization collapses the caller's stack frame before checking
// whether the callee yielded, which corrupts the return-value/resume
// bookkeeping for a G-function that yields from that position. Always
// bind to a local first: "local r = f(...); return r". This applies
// throughout this file and every workflow script.
const luaPrelude = `
function pipeline(items, mapFn, thenFn)
  local fns = {}
  for i, item in ipairs(items) do
    fns[i] = function()
      local r = mapFn(item)
      if thenFn then
        local r2 = thenFn(r)
        return r2
      end
      return r
    end
  end
  local results = parallel(fns)
  return results
end
`

// registerPrimitives binds the workflow script's primitives (phase,
// log, agent, parallel) as Lua globals on L. pipeline is added
// separately by loading luaPrelude.
func registerPrimitives(L *lua.LState, s *scheduler) {
	L.SetGlobal("phase", L.NewFunction(s.luaPhase))
	L.SetGlobal("log", L.NewFunction(s.luaLog))
	L.SetGlobal("agent", L.NewFunction(s.luaAgent))
	L.SetGlobal("parallel", L.NewFunction(s.luaParallel))
}

// luaPhase implements phase(name): marks the workflow's current
// pipeline stage and emits a progress event. Calls to agent() without
// an explicit opts.phase fall back to this value.
func (s *scheduler) luaPhase(co *lua.LState) int {
	name := co.CheckString(1)
	s.currentPhase = name
	s.emitProgress(ProgressEvent{Phase: name})
	return 0
}

// luaLog implements log(msg): emits a progress log line.
func (s *scheduler) luaLog(co *lua.LState) int {
	msg := co.CheckString(1)
	s.emitProgress(ProgressEvent{Log: msg})
	return 0
}

// luaAgent implements agent(prompt, opts): dispatches one sub-agent
// turn. opts is an optional table with string fields `label`,
// `phase`, `model` (see AgentRequest.Model), and a `schema` table
// requesting structured output. It always yields the calling
// coroutine — the result becomes the return value of the agent(...)
// call once the scheduler resumes it with the (Lua-converted) outcome
// — except when the run's agent-call budget has been exhausted, in
// which case it returns nil synchronously without dispatching any
// work, matching how workflow scripts already treat a
// failed/skipped call.
func (s *scheduler) luaAgent(co *lua.LState) int {
	prompt := co.CheckString(1)

	var label, phase, model, schemaName string
	var schema *Schema
	if optsVal := co.Get(2); optsVal != lua.LNil {
		opts, ok := optsVal.(*lua.LTable)
		if !ok {
			co.ArgError(2, "opts must be a table")
		}
		label = lua.LVAsString(opts.RawGetString("label"))
		phase = lua.LVAsString(opts.RawGetString("phase"))
		model = lua.LVAsString(opts.RawGetString("model"))
		if schemaVal := opts.RawGetString("schema"); schemaVal != lua.LNil {
			schema = schemaFromLua(schemaVal)
			schemaName = label
			if schemaName == "" {
				schemaName = "result"
			}
		}
	}
	if phase == "" {
		phase = s.currentPhase
	}

	s.agentCalls++
	if s.agentCalls > s.budget.MaxAgentCalls {
		// LFalse, not LNil, so this result is safe to place in a
		// parallel()/pipeline() results array without creating a hole
		// that would break ipairs on later elements.
		co.Push(lua.LFalse)
		return 1
	}

	s.queue = append(s.queue, queuedAgent{
		co:         co,
		req:        AgentRequest{Prompt: prompt, Label: label, Phase: phase, Model: model, Seq: s.agentCalls},
		schema:     schema,
		schemaName: schemaName,
	})

	return co.Yield()
}

// luaParallel implements parallel(fns): fns is an array of zero-arg
// functions. Each is run on its own coroutine; parallel() yields the
// calling coroutine until every branch has finished (or errored,
// which resolves that branch's slot to nil rather than aborting the
// whole run), then resumes it with an array table of results in the
// original order.
func (s *scheduler) luaParallel(co *lua.LState) int {
	fns := co.CheckTable(1)
	n := fns.Len()
	if n == 0 {
		co.Push(s.L.NewTable())
		return 1
	}

	g := &group{parent: co, results: make([]lua.LValue, n), remaining: n}
	for i := 1; i <= n; i++ {
		fn, ok := fns.RawGetInt(i).(*lua.LFunction)
		if !ok {
			g.results[i-1] = lua.LFalse
			g.remaining--
			continue
		}
		s.spawnChild(g, i-1, fn)
	}

	if g.remaining == 0 {
		tbl := s.L.NewTable()
		for i, v := range g.results {
			tbl.RawSetInt(i+1, v)
		}
		co.Push(tbl)
		return 1
	}

	return co.Yield()
}
