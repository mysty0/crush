package app

import (
	"context"
	"sync"
	"testing"
	"time"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestBashProgress_LocalModeReachesSubscriber reproduces the local
// (AppWorkspace) event path: the bash tool publishes a progress event on
// the tools broker, setupEvents fans it into app.events, and a subscriber
// (standing in for the TUI program.Send) must receive it as a
// pubsub.Event[agenttools.BashProgressEvent].
func TestBashProgress_LocalModeReachesSubscriber(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := NewForTest(ctx)
	t.Cleanup(app.ShutdownForTest)

	// Wire the bash-progress fan-in exactly like production setupEvents.
	setupSubscriber(app.eventsCtx, app.serviceEventsWG, "bash-progress",
		agenttools.SubscribeBashProgress, app.events)

	// Subscribe as the TUI would (app.Subscribe sends ev.Payload).
	sub := app.Events(ctx)

	got := make(chan agenttools.BashProgressEvent, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range sub {
			if bp, ok := ev.Payload.(pubsub.Event[agenttools.BashProgressEvent]); ok {
				got <- bp.Payload
			}
		}
	}()

	// Publish after subscription is active. Retry because the
	// tools->app fan-in subscription attaches asynchronously.
	require.Eventually(t, func() bool {
		agenttools.PublishBashProgress("sess-1", "tc-9", "partial output")
		select {
		case ev := <-got:
			require.Equal(t, "sess-1", ev.SessionID)
			require.Equal(t, "tc-9", ev.ToolCallID)
			require.Equal(t, "partial output", ev.Output)
			return true
		case <-time.After(20 * time.Millisecond):
			return false
		}
	}, 5*time.Second, 10*time.Millisecond,
		"bash progress must reach the app.Events subscriber in local mode")

	cancel()
	wg.Wait()
}
