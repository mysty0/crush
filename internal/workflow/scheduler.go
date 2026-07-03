package workflow

import (
	"context"
	"log/slog"

	lua "github.com/yuin/gopher-lua"
)

// queuedAgent is a pending agent() call waiting for a concurrency
// slot to free up before it is dispatched as a background goroutine.
type queuedAgent struct {
	co         *lua.LState
	req        AgentRequest
	schema     *Schema
	schemaName string
}

// completion carries the result of one finished agent() call back to
// the scheduler's driver loop, to resume the coroutine that issued
// it. The result is a plain Go value (produced off the driver
// goroutine in execute), NOT a lua.LValue: converting to Lua touches
// the shared *lua.LState, which is not goroutine-safe, so that
// conversion must happen on the driver goroutine only.
type completion struct {
	co     *lua.LState
	result any
	// ok reports whether the call produced a usable result. When
	// false (agent error, coercion failure, or budget/skip), the
	// coroutine is resumed with Lua false so scripts can detect it via
	// `if not r`.
	ok bool
}

// group tracks a parallel() (or pipeline(), which is implemented in
// terms of parallel — see luaPrelude) fan-out: N child coroutines
// whose results are collected, in original order, and delivered back
// to the parent coroutine once all have finished.
type group struct {
	parent    *lua.LState
	results   []lua.LValue
	remaining int
}

// groupMember records which group (and index within it) a spawned
// child coroutine belongs to, so its result can be folded in when it
// finishes.
type groupMember struct {
	group *group
	index int
}

// scheduler drives every Resume() call for a workflow run from a
// single goroutine (required: *lua.LState is not goroutine-safe and
// coroutines sharing a *Global cannot be Resumed concurrently). Real
// concurrency comes from background goroutines performing the actual
// agent() work (LLM/network calls); the scheduler only ever touches
// Lua state from the goroutine that calls run().
type scheduler struct {
	ctx      context.Context
	L        *lua.LState
	runner   Runner
	progress ProgressFunc
	budget   Budget

	agentCalls  int
	activeSlots int
	queue       []queuedAgent
	members     map[*lua.LState]*groupMember

	completions chan completion

	currentPhase string

	mainCo      *lua.LState
	mainDone    bool
	mainResults []lua.LValue
	mainErr     error
}

func newScheduler(ctx context.Context, L *lua.LState, runner Runner, progress ProgressFunc, budget Budget) *scheduler {
	return &scheduler{
		ctx:         ctx,
		L:           L,
		runner:      runner,
		progress:    progress,
		budget:      budget,
		members:     make(map[*lua.LState]*groupMember),
		completions: make(chan completion, 1),
	}
}

// run drives the workflow's main coroutine to completion, dispatching
// and servicing agent() calls and parallel() groups as they yield and
// resolve, until the main coroutine returns or errors.
func (s *scheduler) run(mainCo *lua.LState, fn *lua.LFunction) ([]lua.LValue, error) {
	s.mainCo = mainCo
	s.resume(mainCo, fn)
	s.dispatchQueue()

	for !s.mainDone {
		select {
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		case c := <-s.completions:
			s.activeSlots--
			// Convert the raw Go result to a Lua value HERE, on the
			// driver goroutine — never in execute(), which runs
			// concurrently and must not touch the shared *lua.LState.
			var v lua.LValue = lua.LFalse
			if c.ok {
				v = toLua(s.L, c.result)
			}
			s.resume(c.co, nil, v)
			s.dispatchQueue()
		}
	}
	return s.mainResults, s.mainErr
}

// resume performs one Resume() call on co and routes the outcome. A
// coroutine that yields has already registered whatever pending work
// it needs (via the agent/parallel primitives, which run
// synchronously as part of this same Resume call) before yielding, so
// the ResumeYield case has nothing further to do.
func (s *scheduler) resume(co *lua.LState, fn *lua.LFunction, args ...lua.LValue) {
	state, err, vals := s.L.Resume(co, fn, args...)
	switch state {
	case lua.ResumeError:
		s.finish(co, nil, err)
	case lua.ResumeOK:
		s.finish(co, vals, nil)
	case lua.ResumeYield:
	}
}

// finish handles a coroutine reaching ResumeOK or ResumeError: the
// main coroutine's outcome ends the run; a group member's outcome is
// folded into its group's results and, once every member has
// finished, unblocks the group's parent coroutine with the combined
// results table.
func (s *scheduler) finish(co *lua.LState, vals []lua.LValue, err error) {
	if co == s.mainCo {
		s.mainDone = true
		s.mainResults = vals
		s.mainErr = err
		return
	}

	member, ok := s.members[co]
	if !ok {
		return
	}
	delete(s.members, co)

	// LFalse, not LNil, marks a failed branch: nil would be dropped
	// when this result lands in a results array (Lua tables represent
	// absence as "no key", which breaks ipairs on later elements),
	// while false is a real value the script sees via `if not x`.
	var result lua.LValue = lua.LFalse
	switch {
	case err != nil:
		slog.Warn("Workflow branch failed", "error", err)
	case len(vals) > 0:
		result = vals[0]
	}
	member.group.results[member.index] = result
	member.group.remaining--
	if member.group.remaining == 0 {
		tbl := s.L.NewTable()
		for i, v := range member.group.results {
			tbl.RawSetInt(i+1, v)
		}
		s.resume(member.group.parent, nil, tbl)
	}
}

// dispatchQueue starts queued agent() calls as background goroutines
// up to the concurrency budget. Called after every batch of
// synchronous Resume() activity settles, so newly queued calls
// (including ones queued by branches spawned during that activity)
// are picked up.
func (s *scheduler) dispatchQueue() {
	for s.activeSlots < s.budget.MaxConcurrency && len(s.queue) > 0 {
		item := s.queue[0]
		s.queue = s.queue[1:]
		s.activeSlots++
		go s.execute(item)
	}
}

// execute runs one queued agent call in the background: the agentic
// sub-agent turn, then (if a schema was requested) a structured
// output coercion pass. Failures resolve the call as "not ok" rather
// than aborting the run, matching the resilience patterns workflow
// scripts are written to expect (a failed search or fetch is one
// missing data point, not a fatal error).
//
// This runs on a background goroutine and MUST NOT touch the shared
// *lua.LState: it returns a plain Go value, and the driver goroutine
// converts it to Lua (see run()).
func (s *scheduler) execute(item queuedAgent) {
	var (
		result any
		ok     bool
	)
	text, err := s.runner.RunAgent(s.ctx, item.req)
	switch {
	case err != nil:
		slog.Warn("Workflow agent call failed", "label", item.req.Label, "error", err)
	case item.schema != nil:
		obj, cerr := s.runner.CoerceObject(s.ctx, text, item.schema, item.schemaName)
		if cerr != nil {
			slog.Warn("Workflow structured output coercion failed", "label", item.req.Label, "error", cerr)
		} else {
			result, ok = obj, true
		}
	default:
		result, ok = text, true
	}

	select {
	case s.completions <- completion{co: item.co, result: result, ok: ok}:
	case <-s.ctx.Done():
	}
}

// spawnChild starts a fresh child coroutine for a parallel() branch
// and registers it as a member of g at the given index, so its
// eventual result is folded into g by finish().
func (s *scheduler) spawnChild(g *group, index int, fn *lua.LFunction, args ...lua.LValue) {
	child, _ := s.L.NewThread()
	s.members[child] = &groupMember{group: g, index: index}
	s.resume(child, fn, args...)
}

// emitProgress forwards a progress event to the configured callback,
// if any.
func (s *scheduler) emitProgress(event ProgressEvent) {
	if s.progress != nil {
		s.progress(event)
	}
}
