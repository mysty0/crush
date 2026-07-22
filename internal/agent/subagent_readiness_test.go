package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestWaitReadyUnarmedIsReady verifies an agent that was never armed is
// treated as ready, so non-task agents never block in Run.
func TestWaitReadyUnarmedIsReady(t *testing.T) {
	t.Parallel()
	a := &sessionAgent{}
	require.NoError(t, a.WaitReady(context.Background()))
}

// TestWaitReadyBlocksUntilInitDone reproduces the sub-agent race: WaitReady
// must block while asynchronous initialization is pending and unblock only
// once every armed init task has completed.
func TestWaitReadyBlocksUntilInitDone(t *testing.T) {
	t.Parallel()
	a := &sessionAgent{}
	done := a.ArmReady(2)

	// With one of two tasks still pending, WaitReady must not return.
	done()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, a.WaitReady(ctx), context.DeadlineExceeded)

	// Completing the last task releases the barrier.
	done()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	require.NoError(t, a.WaitReady(ctx2))
}

// TestWaitReadyRespectsContext verifies a blocked Run can still be canceled
// while waiting for readiness.
func TestWaitReadyRespectsContext(t *testing.T) {
	t.Parallel()
	a := &sessionAgent{}
	a.ArmReady(1) // Never completed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, a.WaitReady(ctx), context.Canceled)
}

// TestArmReadyExtraDoneCallsAreSafe verifies over-calling done never panics
// or re-closes the channel.
func TestArmReadyExtraDoneCallsAreSafe(t *testing.T) {
	t.Parallel()
	a := &sessionAgent{}
	done := a.ArmReady(1)
	done()
	done() // Extra call must be a no-op, not a double close.
	require.NoError(t, a.WaitReady(context.Background()))
}

// TestLooksLikeNarratedToolCalls covers the degenerate-output detector used
// as defense in depth when a sub-agent narrates tool calls as prose.
func TestLooksLikeNarratedToolCalls(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"anthropic xml block", "I'll read it.\n<function_calls>\n<invoke name=\"read_file\">", true},
		{"invoke only", "<invoke name=\"bash\">", true},
		{"parameter markup", "<parameter name=\"path\">/tmp/x</parameter>", true},
		{"normal report", "# Report\nI read the file and found 3 issues.", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, looksLikeNarratedToolCalls(tc.text))
		})
	}
}

// TestSubAgentToolCallCount verifies tool calls are tallied across all steps
// so a genuinely tool-less run is distinguishable from a working one.
func TestSubAgentToolCallCount(t *testing.T) {
	t.Parallel()
	require.Equal(t, 0, subAgentToolCallCount(nil))
	require.Equal(t, 0, subAgentToolCallCount(&fantasy.AgentResult{}))

	result := &fantasy.AgentResult{
		Steps: []fantasy.StepResult{
			{Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.ToolCallContent{ToolCallID: "1", ToolName: "Read"},
			}}},
			{Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.ToolCallContent{ToolCallID: "2", ToolName: "Grep"},
				fantasy.ToolCallContent{ToolCallID: "3", ToolName: "Edit"},
			}}},
		},
	}
	require.Equal(t, 3, subAgentToolCallCount(result))
}
