package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestPublishBashProgress verifies that incremental bash output published
// during execution is delivered to subscribers.
func TestPublishBashProgress(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub := SubscribeBashProgress(ctx)

	PublishBashProgress("session-1", "tc-1", "line one\nline two")

	event := <-sub
	require.Equal(t, "session-1", event.Payload.SessionID)
	require.Equal(t, "tc-1", event.Payload.ToolCallID)
	require.Equal(t, "line one\nline two", event.Payload.Output)
}

// TestBashTool_StreamsProgressDuringExecution runs a slow multi-line
// command and asserts that progress events are published *before* the
// command finishes. This reproduces the real streaming path end-to-end
// through the bash tool (not just the broker).
func TestBashTool_StreamsProgressDuringExecution(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	progressCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := SubscribeBashProgress(progressCtx)

	// Collect progress events in the background.
	events := make(chan BashProgressEvent, 128)
	go func() {
		for ev := range sub {
			select {
			case events <- ev.Payload:
			default:
			}
		}
	}()

	// A command that emits several lines over ~2 seconds.
	input := `{"command":"for i in 1 2 3 4 5; do echo line $i; sleep 0.4; done"}`
	call := fantasy.ToolCall{ID: "stream-call", Name: BashToolName, Input: input}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err)
	require.False(t, resp.IsError)

	// Drain a beat for any final event still in flight.
	time.Sleep(200 * time.Millisecond)

	var progressOutputs []string
	for {
		select {
		case ev := <-events:
			require.Equal(t, "stream-call", ev.ToolCallID)
			progressOutputs = append(progressOutputs, ev.Output)
			continue
		default:
		}
		break
	}

	require.NotEmpty(t, progressOutputs,
		"expected at least one progress event during a multi-second command")

	// At least one intermediate event should carry partial (not full)
	// output — proving output streams incrementally rather than only at
	// the end.
	sawPartial := false
	for _, out := range progressOutputs {
		if len(out) > 0 && !strings.Contains(out, "line 5") {
			sawPartial = true
			break
		}
	}
	require.True(t, sawPartial,
		"expected an intermediate event with partial output before completion; got %v", progressOutputs)
}
