package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// AgentRequest describes one agent() call issued by a workflow
// script.
type AgentRequest struct {
	// Prompt is the full prompt text passed to the sub-agent.
	Prompt string
	// Label identifies the call for progress/session-title purposes
	// (e.g. "search:academic", "v0:claim text...").
	Label string
	// Phase is the workflow phase active when the call was made, if
	// any (set via phase(name) before the call).
	Phase string
	// Model optionally overrides which model runs this call. Empty
	// uses the workflow's default model; "small" uses the run's
	// configured fast/cheap model; any other non-empty value is
	// treated as an explicit model ID. Runner implementations resolve
	// this the same way the "agent" tool resolves its own "model"
	// parameter.
	Model string
	// Seq is a monotonic, run-scoped sequence number identifying this
	// call, distinct from every other agent() call in the same run
	// (including concurrent ones). Runner implementations can use it
	// to derive a unique child-session ID per call.
	Seq int
}

// Runner executes the actual work behind a workflow script's agent()
// calls. Implemented by internal/agent (the coordinator), and kept as
// an interface here so the engine package has no dependency on
// sessions, permissions, or model configuration.
type Runner interface {
	// RunAgent executes one sub-agent turn (agentic, tool-enabled,
	// using the workflow's fixed tool policy) and returns its final
	// text output.
	RunAgent(ctx context.Context, req AgentRequest) (string, error)

	// CoerceObject forces freeform text into the given JSON schema
	// using the small/fast model, returning a decoded Go value
	// (map[string]any, []any, or a primitive) ready for use as a
	// Lua table.
	CoerceObject(ctx context.Context, text string, schema *Schema, schemaName string) (any, error)
}

// ProgressEvent is emitted by a running workflow via phase()/log()
// calls in the script.
type ProgressEvent struct {
	// Phase is set when the event came from phase(name).
	Phase string
	// Log is set when the event came from log(msg).
	Log string
}

// ProgressFunc receives progress events from a running workflow. It
// may be nil, in which case progress is discarded.
type ProgressFunc func(ProgressEvent)

// Budget bounds the cost of a single workflow run. These are
// engine-enforced ceilings independent of whatever limits a script's
// own logic imposes, so a buggy or malicious script cannot run away.
type Budget struct {
	// MaxAgentCalls caps the total number of agent() calls across the
	// whole run. Zero means DefaultMaxAgentCalls.
	MaxAgentCalls int
	// MaxConcurrency caps the number of agent() calls in flight at
	// once. Zero means DefaultMaxConcurrency.
	MaxConcurrency int
	// Timeout bounds the wall-clock duration of the whole run. Zero
	// means DefaultTimeout.
	Timeout time.Duration
}

// Default budget values, applied when the corresponding Budget field
// is zero.
const (
	DefaultMaxAgentCalls  = 200
	DefaultMaxConcurrency = 16
	DefaultTimeout        = 15 * time.Minute
)

func (b Budget) withDefaults() Budget {
	if b.MaxAgentCalls <= 0 {
		b.MaxAgentCalls = DefaultMaxAgentCalls
	}
	if b.MaxConcurrency <= 0 {
		b.MaxConcurrency = DefaultMaxConcurrency
	}
	if b.Timeout <= 0 {
		b.Timeout = DefaultTimeout
	}
	return b
}

// RunOptions configures a single workflow run.
type RunOptions struct {
	// Script is the Lua source to execute.
	Script string
	// Args is the freeform argument string passed to the workflow as
	// the global `args`.
	Args string
	// Runner executes agent() calls. Required.
	Runner Runner
	// Progress receives phase()/log() events. Optional.
	Progress ProgressFunc
	// Budget bounds the run's cost. Zero values fall back to
	// defaults.
	Budget Budget
}

// Errors returned by Run.
var (
	ErrRunnerRequired = errors.New("workflow: Runner is required")
	ErrBudgetExceeded = errors.New("workflow: agent call budget exceeded")
)

// Run executes a workflow script to completion and returns its final
// return value converted to a plain Go value (map[string]any, []any,
// or a primitive), ready for JSON encoding.
func Run(ctx context.Context, opts RunOptions) (any, error) {
	if opts.Runner == nil {
		return nil, ErrRunnerRequired
	}
	budget := opts.Budget.withDefaults()

	ctx, cancel := context.WithTimeout(ctx, budget.Timeout)
	defer cancel()

	L, err := newSandboxedState()
	if err != nil {
		return nil, fmt.Errorf("workflow: create lua state: %w", err)
	}
	defer L.Close()

	sched := newScheduler(ctx, L, opts.Runner, opts.Progress, budget)
	registerPrimitives(L, sched)
	L.SetGlobal("args", lua.LString(opts.Args))

	fn, err := L.LoadString(luaPrelude + opts.Script)
	if err != nil {
		return nil, fmt.Errorf("workflow: parse script: %w", err)
	}

	mainCo, _ := L.NewThread()
	results, err := sched.run(mainCo, fn)
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, nil
	}
	return toGo(results[0]), nil
}

// newSandboxedState creates a *lua.LState with only safe base
// libraries loaded: no filesystem, network, process, eval, or raw
// coroutine access. Scripts get exactly the primitives registered by
// registerPrimitives for anything resembling I/O or concurrency.
func newSandboxedState() (*lua.LState, error) {
	L := lua.NewState(lua.Options{
		SkipOpenLibs:        true,
		IncludeGoStackTrace: true,
	})

	lua.OpenBase(L)
	// Strip dangerous base functions that OpenBase registers but a
	// sandboxed script must not have: arbitrary code loading/eval,
	// filesystem access, and function-environment tampering.
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
	// Deliberately not opened: package/require, io, os, debug,
	// channel, coroutine. Concurrency and "I/O" (agent calls) are
	// exposed only through the primitives in primitives.go.

	return L, nil
}
