package model

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAgentToolChatItem builds a running "agent" tool-call chat item whose
// input JSON encodes the given AgentParams, mirroring what the coder
// session's chat actually contains while a sub-agent turn is in progress.
func newAgentToolChatItem(t *testing.T, sty *styles.Styles, toolCallID string, params agent.AgentParams) chat.MessageItem {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	return chat.NewToolMessageItem(sty, "parent-msg", message.ToolCall{
		ID:    toolCallID,
		Name:  agent.AgentToolName,
		Input: string(input),
	}, nil, false)
}

func newTestUIWithChat(t *testing.T) (*UI, *Chat) {
	t.Helper()
	sty := styles.CharmtonePantera()
	com := &common.Common{
		Workspace: &testWorkspace{},
		Styles:    &sty,
	}
	c := NewChat(com, "")
	return &UI{com: com, chat: c}, c
}

func TestResolveAgentToolCallID(t *testing.T) {
	t.Parallel()

	t.Run("resolves a resumed call by its resume_session_id, not the stale encoded ID", func(t *testing.T) {
		t.Parallel()
		ui, c := newTestUIWithChat(t)

		// The original (now-finished) call that first created the session.
		// Its own tool-call ID is embedded in the session ID itself.
		originalItem := newAgentToolChatItem(t, ui.com.Styles, "original-call", agent.AgentParams{Prompt: "investigate"})
		if setter, ok := originalItem.(interface{ SetStatus(chat.ToolStatus) }); ok {
			setter.SetStatus(chat.ToolStatusCanceled)
		}
		c.AppendMessages(originalItem)

		// The resumed call: a *new* tool-call ID, still running, whose
		// input names the *old* session via resume_session_id.
		const resumedSessionID = "parent-msg$$original-call"
		resumedItem := newAgentToolChatItem(t, ui.com.Styles, "resumed-call", agent.AgentParams{
			Prompt:          "continue",
			ResumeSessionID: resumedSessionID,
		})
		c.AppendMessages(resumedItem)

		toolCallID, ok := ui.resolveAgentToolCallID(resumedSessionID)
		require.True(t, ok)
		// Must route to the currently-running resumed call, not the
		// stale finished one the naive session-ID parse would find.
		assert.Equal(t, "resumed-call", toolCallID)
	})

	t.Run("falls back to the parsed session ID when no resume is in progress", func(t *testing.T) {
		t.Parallel()
		ui, c := newTestUIWithChat(t)
		ui.com.Workspace = &parsingTestWorkspace{messageID: "msg-1", toolCallID: "call-1"}

		item := newAgentToolChatItem(t, ui.com.Styles, "call-1", agent.AgentParams{Prompt: "investigate"})
		c.AppendMessages(item)

		toolCallID, ok := ui.resolveAgentToolCallID("msg-1$$call-1")
		require.True(t, ok)
		assert.Equal(t, "call-1", toolCallID)
	})

	t.Run("ignores a finished call's resume_session_id (a resume is no longer active)", func(t *testing.T) {
		t.Parallel()
		ui, c := newTestUIWithChat(t)
		ui.com.Workspace = &parsingTestWorkspace{messageID: "msg-1", toolCallID: "call-1"}

		const resumedSessionID = "msg-1$$call-1"
		finishedResume := newAgentToolChatItem(t, ui.com.Styles, "resumed-call", agent.AgentParams{
			Prompt:          "continue",
			ResumeSessionID: resumedSessionID,
		})
		if setter, ok := finishedResume.(interface{ SetStatus(chat.ToolStatus) }); ok {
			setter.SetStatus(chat.ToolStatusSuccess)
		}
		c.AppendMessages(finishedResume)

		toolCallID, ok := ui.resolveAgentToolCallID(resumedSessionID)
		require.True(t, ok)
		// The resumed call already finished, so it must not be matched;
		// falls back to the naive parse.
		assert.Equal(t, "call-1", toolCallID)
	})
}

// parsingTestWorkspace stubs ParseAgentToolSessionID for the fallback path.
type parsingTestWorkspace struct {
	testWorkspace
	messageID  string
	toolCallID string
}

func (w *parsingTestWorkspace) ParseAgentToolSessionID(string) (string, string, bool) {
	return w.messageID, w.toolCallID, true
}
