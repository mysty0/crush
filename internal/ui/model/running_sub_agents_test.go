package model

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

// sessionIDTestWorkspace stubs CreateAgentToolSessionID with the same
// "messageID$$toolCallID" format session.Service actually uses, so
// runningSubAgents' derived-session-ID path is exercised realistically.
type sessionIDTestWorkspace struct {
	testWorkspace
}

func (w *sessionIDTestWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return messageID + "$$" + toolCallID
}

// newAgenticFetchToolChatItem builds a running "agentic_fetch" tool-call
// chat item whose input JSON encodes the given params, mirroring what the
// coder session's chat actually contains while a fetch sub-agent is running.
func newAgenticFetchToolChatItem(t *testing.T, ui *UI, toolCallID string, params agenttools.AgenticFetchParams) chat.MessageItem {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	return chat.NewToolMessageItem(ui.com.Styles, "parent-msg", message.ToolCall{
		ID:    toolCallID,
		Name:  agenttools.AgenticFetchToolName,
		Input: string(input),
	}, nil, false)
}

// TestRunningSubAgentsIncludesAgenticFetch verifies that an in-flight
// "agentic_fetch" tool call appears in the bottom agent picker list
// alongside "agent" calls -- both dispatch through the same sub-agent
// machinery and deserve the same "select it to go inside" affordance.
func TestRunningSubAgentsIncludesAgenticFetch(t *testing.T) {
	t.Parallel()
	ui, c := newTestUIWithChat(t)
	ui.com.Workspace = &sessionIDTestWorkspace{}

	agentItem := newAgentToolChatItem(t, ui.com.Styles, "agent-call", agent.AgentParams{Prompt: "investigate the VRAM issue"})
	fetchItem := newAgenticFetchToolChatItem(t, ui, "fetch-call", agenttools.AgenticFetchParams{
		URL:    "https://example.com",
		Prompt: "summarize the release notes",
	})
	c.AppendMessages(agentItem, fetchItem)

	entries := ui.runningSubAgents()
	require.Len(t, entries, 2, "both the running agent call and the running agentic_fetch call must be listed")

	byToolCallID := make(map[string]subAgentEntry, len(entries))
	for _, e := range entries {
		byToolCallID[e.ToolCallID] = e
	}

	agentEntry, ok := byToolCallID["agent-call"]
	require.True(t, ok)
	require.Equal(t, "investigate the VRAM issue", agentEntry.Prompt)
	require.Equal(t, "parent-msg$$agent-call", agentEntry.SessionID)

	fetchEntry, ok := byToolCallID["fetch-call"]
	require.True(t, ok, "agentic_fetch call must be selectable in the picker list, same as agent")
	require.Equal(t, "summarize the release notes", fetchEntry.Prompt)
	require.Equal(t, "parent-msg$$fetch-call", fetchEntry.SessionID)
}

// TestRunningSubAgentsExcludesFinishedAgenticFetch mirrors the existing
// behavior for "agent" calls: once a tool call has a result, it must drop
// out of the picker list instead of lingering as a stale, unselectable-once-
// finished entry.
func TestRunningSubAgentsExcludesFinishedAgenticFetch(t *testing.T) {
	t.Parallel()
	ui, c := newTestUIWithChat(t)
	ui.com.Workspace = &sessionIDTestWorkspace{}

	input, err := json.Marshal(agenttools.AgenticFetchParams{Prompt: "look something up"})
	require.NoError(t, err)
	fetchItem := chat.NewToolMessageItem(ui.com.Styles, "parent-msg", message.ToolCall{
		ID:       "fetch-call",
		Name:     agenttools.AgenticFetchToolName,
		Input:    string(input),
		Finished: true,
	}, &message.ToolResult{ToolCallID: "fetch-call", Content: "done"}, false)
	c.AppendMessages(fetchItem)

	entries := ui.runningSubAgents()
	require.Empty(t, entries, "a finished agentic_fetch call must not appear in the picker list")
}

// TestRunningSubAgentsIgnoresUnrelatedToolCalls guards against
// subAgentEntryFor over-matching: an ordinary tool call (not "agent" or
// "agentic_fetch") must never surface in the picker list.
func TestRunningSubAgentsIgnoresUnrelatedToolCalls(t *testing.T) {
	t.Parallel()
	ui, c := newTestUIWithChat(t)
	ui.com.Workspace = &sessionIDTestWorkspace{}

	bashItem := chat.NewToolMessageItem(ui.com.Styles, "parent-msg", message.ToolCall{
		ID:    "bash-call",
		Name:  "Bash",
		Input: `{"command":"echo hi"}`,
	}, nil, false)
	c.AppendMessages(bashItem)

	entries := ui.runningSubAgents()
	require.Empty(t, entries)
}
