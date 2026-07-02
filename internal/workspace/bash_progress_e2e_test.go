package workspace_test

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestClientWorkspace_BashProgressArrives is the end-to-end test for
// live foreground bash output streaming in client/server mode. It
// publishes a BashProgressEvent through the tools package broker (the
// same broker the running bash tool uses) and asserts the event survives
// the full server -> SSE -> client -> translateEvent round-trip and is
// delivered to the TUI-facing consumer as a translated domain event.
func TestClientWorkspace_BashProgressArrives(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)

	cwd := t.TempDir()
	dataDir := t.TempDir()

	c := rt.newClient(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: cwd, DataDir: dataDir})
	require.NoError(t, err)

	ws := workspace.NewClientWorkspace(c, *wsProto)

	evc, err := c.SubscribeEvents(ctx, wsProto.ID)
	require.NoError(t, err)

	// Drive the client event loop and collect translated messages.
	got := make(chan agenttools.BashProgressEvent, 16)
	go ws.ConsumeEventsForTest(evc, func(msg tea.Msg) {
		if ev, ok := msg.(pubsub.Event[agenttools.BashProgressEvent]); ok {
			got <- ev.Payload
		}
	})

	// Give the server-side subscription a moment to attach before we
	// publish, otherwise the (lossy) broker publish can race ahead of
	// the subscriber.
	require.Eventually(t, func() bool {
		agenttools.PublishBashProgress("sess-1", "tc-42", "partial output line")
		select {
		case ev := <-got:
			require.Equal(t, "sess-1", ev.SessionID)
			require.Equal(t, "tc-42", ev.ToolCallID)
			require.Equal(t, "partial output line", ev.Output)
			return true
		default:
			return false
		}
	}, 5*time.Second, 50*time.Millisecond,
		"bash progress event must reach the client over SSE")
}
