package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func withPollGuardSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, tools.SessionIDContextKey, sessionID)
}

func TestClassifyPollCall(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		tool  string
		input string
		want  pollClass
	}{
		{"bare sleep", tools.BashToolName, `{"command":"sleep 240"}`, pollClassBlockingWait},
		{"sleep with trailing echo", tools.BashToolName, `{"command":"sleep 240; echo done"}`, pollClassBlockingWait},
		{"sleep with && echo", tools.BashToolName, `{"command":"sleep 5 && echo waited"}`, pollClassBlockingWait},
		{"backgrounded sleep launch", tools.BashToolName, `{"command":"sleep 240; echo done","run_in_background":true}`, pollClassCheck},
		{"real bash command", tools.BashToolName, `{"command":"cargo check -p sysmon"}`, pollClassWork},
		{"sleep chained with real work", tools.BashToolName, `{"command":"sleep 5; cargo build"}`, pollClassWork},
		{"unparseable bash input", tools.BashToolName, `not json`, pollClassWork},
		{"job_output wait true", tools.JobOutputToolName, `{"shell_id":"1","wait":true}`, pollClassBlockingWait},
		{"job_output wait false", tools.JobOutputToolName, `{"shell_id":"1","wait":false}`, pollClassCheck},
		{"job_output no wait field", tools.JobOutputToolName, `{"shell_id":"1"}`, pollClassCheck},
		{"AgentProgress", "AgentProgress", `{"session_id":"x"}`, pollClassCheck},
		{"AgentList", "AgentList", `{}`, pollClassCheck},
		{"unrelated tool", "Edit", `{"file_path":"x"}`, pollClassWork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyPollCall(tc.tool, tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestPollGuardedTool_BlocksSecondConsecutiveWait reproduces the incident
// this guard exists to prevent: a session backgrounds a task, then loops
// sleep(run_in_background=true) -> job_output(wait=true) -> AgentProgress
// repeatedly instead of either doing other work or trusting automatic
// delivery. Launching a new background sleep and checking progress never
// block (they're cheap or instant), so a naive implementation that reset
// the streak on the sleep launch would defeat itself -- re-arming the
// timer right before every wait would erase the streak each cycle. The
// first wait must be allowed (there's nothing wrong with checking once),
// but the second consecutive one must be refused.
func TestPollGuardedTool_BlocksSecondConsecutiveWait(t *testing.T) {
	t.Parallel()

	guard := newPollGuard()
	sleepInner := &fakeTool{name: tools.BashToolName, resp: fantasy.NewTextResponse("started shell 42")}
	sleepTool := &pollGuardedTool{inner: sleepInner, guard: guard}

	waitInner := &fakeTool{name: tools.JobOutputToolName, resp: fantasy.NewTextResponse("still running")}
	waitTool := &pollGuardedTool{inner: waitInner, guard: guard}

	progressInner := &fakeTool{name: "AgentProgress", resp: fantasy.NewTextResponse("50% done")}
	progressTool := &pollGuardedTool{inner: progressInner, guard: guard}

	ctx := withPollGuardSession(t.Context(), "sess-1")

	// Cycle 1: launch the background sleep, wait on it, check progress.
	// All allowed -- this is a single, reasonable check.
	_, err := sleepTool.Run(ctx, fantasy.ToolCall{Name: tools.BashToolName, Input: `{"command":"sleep 240; echo done","run_in_background":true}`})
	require.NoError(t, err)
	require.True(t, sleepInner.called, "launching a background sleep never blocks, should always run")

	resp, err := waitTool.Run(ctx, fantasy.ToolCall{Name: tools.JobOutputToolName, Input: `{"shell_id":"42","wait":true,"timeout":240}`})
	require.NoError(t, err)
	require.True(t, waitInner.called, "first blocking wait should be allowed")
	require.False(t, resp.IsError)

	waitInner.called = false
	_, err = progressTool.Run(ctx, fantasy.ToolCall{Name: "AgentProgress", Input: `{"session_id":"sub-1"}`})
	require.NoError(t, err)
	require.True(t, progressInner.called, "free progress check should always be allowed")

	// Cycle 2: without any real work in between, re-arm the sleep and
	// wait again. The sleep re-arm still runs (it's cheap and doesn't
	// block), but the second consecutive blocking wait must be refused.
	sleepInner.called = false
	_, err = sleepTool.Run(ctx, fantasy.ToolCall{Name: tools.BashToolName, Input: `{"command":"sleep 240; echo done","run_in_background":true}`})
	require.NoError(t, err)
	require.True(t, sleepInner.called, "re-arming the background sleep still doesn't block, so it still runs")

	resp, err = waitTool.Run(ctx, fantasy.ToolCall{Name: tools.JobOutputToolName, Input: `{"shell_id":"42","wait":true,"timeout":240}`})
	require.NoError(t, err)
	require.False(t, waitInner.called, "second consecutive blocking wait must be refused")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "was not executed")
}

// TestPollGuardedTool_RealWorkResetsStreak confirms a session that does
// real work between waits -- reading a file, editing, running a real
// command -- is never penalized, no matter how many times it checks back.
func TestPollGuardedTool_RealWorkResetsStreak(t *testing.T) {
	t.Parallel()

	guard := newPollGuard()
	waitInner := &fakeTool{name: tools.JobOutputToolName, resp: fantasy.NewTextResponse("still running")}
	waitTool := &pollGuardedTool{inner: waitInner, guard: guard}

	editInner := &fakeTool{name: "Edit", resp: fantasy.NewTextResponse("edited")}
	editTool := &pollGuardedTool{inner: editInner, guard: guard}

	ctx := withPollGuardSession(t.Context(), "sess-2")

	for i := 0; i < 5; i++ {
		waitInner.called = false
		resp, err := waitTool.Run(ctx, fantasy.ToolCall{Name: tools.JobOutputToolName, Input: `{"shell_id":"9","wait":true,"timeout":30}`})
		require.NoError(t, err)
		require.True(t, waitInner.called, "wait after real work should always be allowed, iteration %d", i)
		require.False(t, resp.IsError)

		_, err = editTool.Run(ctx, fantasy.ToolCall{Name: "Edit", Input: `{"file_path":"x.go"}`})
		require.NoError(t, err)
	}
}

// TestPollGuardedTool_SessionsAreIndependent confirms one session's
// blocking-wait streak cannot cause another session's calls to be refused.
func TestPollGuardedTool_SessionsAreIndependent(t *testing.T) {
	t.Parallel()

	guard := newPollGuard()
	inner := &fakeTool{name: tools.JobOutputToolName, resp: fantasy.NewTextResponse("still running")}
	tool := &pollGuardedTool{inner: inner, guard: guard}

	call := fantasy.ToolCall{Name: tools.JobOutputToolName, Input: `{"shell_id":"1","wait":true}`}

	ctxA := withPollGuardSession(t.Context(), "sess-a")
	ctxB := withPollGuardSession(t.Context(), "sess-b")

	// Exhaust session A's allowance.
	_, err := tool.Run(ctxA, call)
	require.NoError(t, err)
	inner.called = false
	resp, err := tool.Run(ctxA, call)
	require.NoError(t, err)
	require.False(t, inner.called)
	require.True(t, resp.IsError)

	// Session B is unaffected.
	inner.called = false
	resp, err = tool.Run(ctxB, call)
	require.NoError(t, err)
	require.True(t, inner.called, "a different session's streak must not affect this one")
	require.False(t, resp.IsError)
}
