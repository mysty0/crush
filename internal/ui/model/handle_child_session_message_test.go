package model

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleChildSessionMessage_ResumedCall reproduces the full live-update
// pipeline for a resumed sub-agent call: a first "agent" tool call is
// dispatched, receives some nested tool-call progress, then (simulating a
// cancel) a second "agent" tool call is dispatched with resume_session_id
// pointing at the *same* underlying session. A further nested tool-call
// event for that session must land on the new (resumed) chat item, not the
// original one -- and the new item must actually receive it, not silently
// drop the update.
func TestHandleChildSessionMessage_ResumedCall(t *testing.T) {
	t.Parallel()
	ui, _ := newTestUIWithChat(t)
	ui.com.Workspace = &parsingTestWorkspace{messageID: "asst-msg-1", toolCallID: "original-call"}

	const childSessionID = "asst-msg-1$$original-call"

	// 1. The original dispatch: a new "agent" tool call appears in the
	// coder session's own assistant message, exactly as updateSessionMessage
	// handles it for a live-streaming turn.
	original := message.Message{ID: "asst-msg-1", Role: message.Assistant}
	original.AddToolCall(message.ToolCall{
		ID: "original-call", Name: agent.AgentToolName,
		Input: marshalParams(t, agent.AgentParams{Prompt: "investigate"}), Finished: true,
	})
	ui.updateSessionMessage(original)

	originalItem, ok := ui.chat.MessageItem("original-call").(chat.NestedToolContainer)
	require.True(t, ok, "original agent tool item should exist in the chat")

	// 2. Nested progress streams in for the original session.
	ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type: pubsub.UpdatedEvent,
		Payload: withToolCall(message.Message{ID: "child-msg-1", SessionID: childSessionID, Role: message.Assistant},
			message.ToolCall{ID: "nested-1", Name: "Bash", Input: "{}", Finished: true}),
	})
	require.Len(t, originalItem.NestedTools(), 1, "the original item should have received its own nested progress")
	assert.Equal(t, "nested-1", originalItem.NestedTools()[0].ToolCall().ID)

	// 3. Simulate a cancel (no explicit action needed -- the original run
	// just stops producing events) and a resumed dispatch: a *new*
	// "agent" tool call, with resume_session_id pointing at the same
	// underlying session the original one used.
	resumed := message.Message{ID: "asst-msg-2", Role: message.Assistant}
	resumed.AddToolCall(message.ToolCall{
		ID: "resumed-call", Name: agent.AgentToolName,
		Input: marshalParams(t, agent.AgentParams{Prompt: "continue", ResumeSessionID: childSessionID}), Finished: true,
	})
	ui.updateSessionMessage(resumed)

	resumedItem, ok := ui.chat.MessageItem("resumed-call").(chat.NestedToolContainer)
	require.True(t, ok, "resumed agent tool item should exist in the chat")
	require.Len(t, resumedItem.NestedTools(), 1, "the resumed item should start seeded with the prior run's nested tools, not blank")
	assert.Equal(t, "nested-1", resumedItem.NestedTools()[0].ToolCall().ID)

	// 4. Further nested progress arrives for the *same* (reused) session
	// ID. It must land on the resumed item, not the original one, and it
	// must not be silently dropped.
	ui.handleChildSessionMessage(pubsub.Event[message.Message]{
		Type: pubsub.UpdatedEvent,
		Payload: withToolCall(message.Message{ID: "child-msg-2", SessionID: childSessionID, Role: message.Assistant},
			message.ToolCall{ID: "nested-2", Name: "View", Input: "{}", Finished: true}),
	})

	require.Len(t, resumedItem.NestedTools(), 2, "the resumed item must keep the seeded history and receive the post-resume nested progress, not silently drop it")
	assert.Equal(t, "nested-1", resumedItem.NestedTools()[0].ToolCall().ID)
	assert.Equal(t, "nested-2", resumedItem.NestedTools()[1].ToolCall().ID)

	// The original (now-stale) item must not have absorbed the update
	// meant for the resumed one.
	assert.Len(t, originalItem.NestedTools(), 1, "the original item must not receive updates meant for the resumed call")
}

func marshalParams(t *testing.T, params agent.AgentParams) string {
	t.Helper()
	b, err := json.Marshal(params)
	require.NoError(t, err)
	return string(b)
}

func withToolCall(msg message.Message, tc message.ToolCall) message.Message {
	msg.AddToolCall(tc)
	return msg
}
