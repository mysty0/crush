package workflow

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// nestedRunner returns a deeply-nested structured object for every
// agent() call, forcing the driver goroutine to build many Lua tables
// via toLua while background execute goroutines run concurrently. This
// is the scenario that would corrupt the shared *lua.LState (and
// freeze the host session) if toLua were ever called off the driver
// goroutine, so it is run under -race in CI.
type nestedRunner struct {
	calls atomic.Int64
}

func (r *nestedRunner) RunAgent(_ context.Context, req AgentRequest) (string, error) {
	r.calls.Add(1)
	return "text:" + req.Label, nil
}

func (r *nestedRunner) CoerceObject(_ context.Context, text string, _ *Schema, _ string) (any, error) {
	return map[string]any{
		"summary": text,
		"items": []any{
			map[string]any{"a": 1.0, "b": "x", "nested": []any{1.0, 2.0, 3.0}},
			map[string]any{"a": 2.0, "b": "y", "nested": []any{4.0, 5.0}},
		},
		"meta": map[string]any{"ok": true, "deep": map[string]any{"k": "v"}},
	}, nil
}

func TestRun_ConcurrentToLuaIsRaceSafe(t *testing.T) {
	t.Parallel()
	runner := &nestedRunner{}
	result, err := Run(t.Context(), RunOptions{
		Script: `
local fns = {}
for i = 1, 40 do
  fns[i] = function()
    local r = agent("q" .. i, {
      label = "branch:" .. i,
      schema = { type = "object", properties = { summary = { type = "string" } } },
    })
    return r
  end
end
local results = parallel(fns)
local n = 0
for _, r in ipairs(results) do
  if r and r.summary then n = n + 1 end
end
return { count = n }
`,
		Runner: runner,
		Budget: Budget{MaxConcurrency: 16},
	})
	require.NoError(t, err)
	m := result.(map[string]any)
	require.Equal(t, float64(40), m["count"])
	require.EqualValues(t, 40, runner.calls.Load())
}
