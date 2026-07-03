package workflow

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mockRunner returns canned/derived responses without any network
// access, and can optionally fail specific labels to exercise
// failure-handling paths.
type mockRunner struct {
	calls       atomic.Int64
	maxInFlight atomic.Int64
	inFlight    atomic.Int64

	failLabels map[string]bool
}

func (m *mockRunner) RunAgent(_ context.Context, req AgentRequest) (string, error) {
	m.calls.Add(1)
	n := m.inFlight.Add(1)
	defer m.inFlight.Add(-1)
	for {
		max := m.maxInFlight.Load()
		if n <= max || m.maxInFlight.CompareAndSwap(max, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)

	if m.failLabels[req.Label] {
		return "", fmt.Errorf("mock failure for %s", req.Label)
	}
	return "response for " + req.Label, nil
}

func (m *mockRunner) CoerceObject(_ context.Context, text string, schema *Schema, _ string) (any, error) {
	// Build a minimal object satisfying whatever the schema asks for,
	// using the input text as a marker so tests can assert on it.
	obj := map[string]any{}
	for k, prop := range schema.Properties {
		switch prop.Type {
		case "array":
			obj[k] = []any{}
		case "boolean":
			obj[k] = false
		case "string":
			obj[k] = text
		default:
			obj[k] = nil
		}
	}
	return obj, nil
}

func TestRun_SimpleAgentCall(t *testing.T) {
	t.Parallel()
	runner := &mockRunner{}
	result, err := Run(t.Context(), RunOptions{
		Script: `
local r = agent("hello", { label = "greet" })
return { echoed = r }
`,
		Runner: runner,
	})
	require.NoError(t, err)
	m, ok := result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "response for greet", m["echoed"])
	require.EqualValues(t, 1, runner.calls.Load())
}

func TestRun_ParallelFanOut(t *testing.T) {
	t.Parallel()
	runner := &mockRunner{}
	result, err := Run(t.Context(), RunOptions{
		Script: `
local fns = {}
for i = 1, 5 do
  fns[i] = function()
    local r = agent("q" .. i, { label = "search:" .. i })
    return r
  end
end
local results = parallel(fns)
return { count = #results, first = results[1] }
`,
		Runner: runner,
		Budget: Budget{MaxConcurrency: 3},
	})
	require.NoError(t, err)
	m := result.(map[string]any)
	require.Equal(t, float64(5), m["count"])
	require.Equal(t, "response for search:1", m["first"])
	require.EqualValues(t, 5, runner.calls.Load())
	// With MaxConcurrency=3 and 5 branches, we should never exceed 3
	// in flight at once, but should exercise more than 1 (i.e. actual
	// concurrency, not serialization).
	require.LessOrEqual(t, runner.maxInFlight.Load(), int64(3))
	require.Greater(t, runner.maxInFlight.Load(), int64(1))
}

func TestRun_PipelineStreams(t *testing.T) {
	t.Parallel()
	runner := &mockRunner{}
	result, err := Run(t.Context(), RunOptions{
		Script: `
local items = {"a", "b", "c"}
local results = pipeline(items,
  function(item)
    local r = agent("map:" .. item, { label = "map:" .. item })
    return r
  end,
  function(mapped)
    local r = agent("then:" .. mapped, { label = "then" })
    return r
  end
)
return { count = #results }
`,
		Runner: runner,
	})
	require.NoError(t, err)
	m := result.(map[string]any)
	require.Equal(t, float64(3), m["count"])
	// 3 map calls + 3 then calls.
	require.EqualValues(t, 6, runner.calls.Load())
}

func TestRun_FailedBranchDoesNotAbortGroup(t *testing.T) {
	t.Parallel()
	runner := &mockRunner{failLabels: map[string]bool{"b": true}}
	result, err := Run(t.Context(), RunOptions{
		Script: `
local fns = {
  function()
    local r = agent("x", { label = "a" })
    return r
  end,
  function()
    local r = agent("x", { label = "b" })
    return r
  end,
  function()
    local r = agent("x", { label = "c" })
    return r
  end,
}
local results = parallel(fns)
local nils = 0
for _, r in ipairs(results) do
  if not r then nils = nils + 1 end
end
return { total = #results, failed = nils }
`,
		Runner: runner,
	})
	require.NoError(t, err)
	m := result.(map[string]any)
	require.Equal(t, float64(3), m["total"], "a false/nil branch result must not create a hole")
	require.Equal(t, float64(1), m["failed"])
}

func TestRun_SchemaCoercion(t *testing.T) {
	t.Parallel()
	runner := &mockRunner{}
	result, err := Run(t.Context(), RunOptions{
		Script: `
local r = agent("give me structured data", {
  label = "structured",
  schema = {
    type = "object",
    required = { "summary" },
    properties = { summary = { type = "string" } },
  },
})
return { summary = r.summary }
`,
		Runner: runner,
	})
	require.NoError(t, err)
	m := result.(map[string]any)
	require.Equal(t, "response for structured", m["summary"])
}

func TestRun_BudgetExceeded(t *testing.T) {
	t.Parallel()
	runner := &mockRunner{}
	result, err := Run(t.Context(), RunOptions{
		Script: `
local fns = {}
for i = 1, 5 do
  fns[i] = function()
    local r = agent("x", { label = "c" .. i })
    return r
  end
end
local results = parallel(fns)
local ok = 0
for _, r in ipairs(results) do
  if r then ok = ok + 1 end
end
return { ok = ok }
`,
		Runner: runner,
		Budget: Budget{MaxAgentCalls: 2},
	})
	require.NoError(t, err)
	m := result.(map[string]any)
	require.Equal(t, float64(2), m["ok"], "only 2 of 5 calls should have been allowed through the budget")
}

func TestRun_NoRunner(t *testing.T) {
	t.Parallel()
	_, err := Run(t.Context(), RunOptions{Script: `return {}`})
	require.ErrorIs(t, err, ErrRunnerRequired)
}

func TestRun_ScriptSyntaxError(t *testing.T) {
	t.Parallel()
	_, err := Run(t.Context(), RunOptions{
		Script: `this is not lua`,
		Runner: &mockRunner{},
	})
	require.Error(t, err)
}

func TestRun_ScriptRuntimeErrorPropagates(t *testing.T) {
	t.Parallel()
	_, err := Run(t.Context(), RunOptions{
		Script: `error("boom")`,
		Runner: &mockRunner{},
	})
	require.Error(t, err)
}

func TestRun_ProgressEvents(t *testing.T) {
	t.Parallel()
	var events []ProgressEvent
	_, err := Run(t.Context(), RunOptions{
		Script: `
phase("Scope")
log("starting")
return {}
`,
		Runner: &mockRunner{},
		Progress: func(e ProgressEvent) {
			events = append(events, e)
		},
	})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "Scope", events[0].Phase)
	require.Equal(t, "starting", events[1].Log)
}

func TestRun_SandboxBlocksDangerousGlobals(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"load", "loadstring", "dofile", "loadfile", "require", "os", "io", "coroutine"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Run(t.Context(), RunOptions{
				Script: fmt.Sprintf(`
if %s ~= nil then
  error("%s should not be available")
end
return {}
`, name, name),
				Runner: &mockRunner{},
			})
			require.NoError(t, err)
		})
	}
}

func TestDiscover_DeepResearch(t *testing.T) {
	t.Parallel()
	w, err := Find("deep-research")
	require.NoError(t, err)
	require.NotNil(t, w)
	require.NotEmpty(t, w.Script)
	require.Equal(t, "built-in", w.Source)
}
