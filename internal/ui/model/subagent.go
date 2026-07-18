package model

import (
	"context"
	"encoding/json"
	"fmt"
	"image"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/util"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/charmbracelet/x/ansi"
)

// subAgentBannerHeight is the number of rows the fullscreen sub-agent
// view reserves at the top of the main content area for its banner.
const subAgentBannerHeight = 2

// maxAgentListPromptLength caps how much of a sub-agent's prompt is
// shown in the agent picker list.
const maxAgentListPromptLength = 60

// activeChat returns the Chat currently shown in the main content
// area: the fullscreen sub-agent chat while one is focused, otherwise
// the main session chat.
func (m *UI) activeChat() *Chat {
	if m.subAgentSessionID != "" {
		return m.subAgentChat
	}
	return m.chat
}

// subAgentMessagesLoadedMsg carries a freshly loaded sub-agent
// session's message history for display in the fullscreen sub-agent
// view. sessionID is checked against m.subAgentSessionID on arrival
// so a stale load (from a view the user has already left) is dropped.
type subAgentMessagesLoadedMsg struct {
	sessionID string
	messages  []message.Message
}

// enterSubAgentView switches the main content area to a fullscreen,
// live view of a running sub-agent's own conversation, and binds the
// prompt input to that sub-agent's session (see sendToSubAgent). Only
// supported while the sub-agent is running; the view auto-exits (see
// appendSessionMessage) once the corresponding "agent" tool call
// finishes.
func (m *UI) enterSubAgentView(sessionID, toolCallID, prompt string) tea.Cmd {
	if m.subAgentChat == nil {
		scrollbarMode := config.ScrollbarDefault
		if cfg := m.com.Config(); cfg.Options.TUI != nil && cfg.Options.TUI.Scrollbar != "" {
			scrollbarMode = cfg.Options.TUI.Scrollbar
		}
		m.subAgentChat = NewChat(m.com, scrollbarMode)
	}
	m.subAgentSessionID = sessionID
	m.subAgentToolCallID = toolCallID
	m.subAgentPrompt = prompt
	m.subAgentChat.ClearMessages()
	m.updateLayoutAndSize()
	return m.loadSubAgentMessages(sessionID)
}

// chat view, or back to the workflow two-pane view if the sub-agent
// was entered from there (see enterWorkflowAgentView).
func (m *UI) exitSubAgentView() {
	m.subAgentSessionID = ""
	m.subAgentToolCallID = ""
	m.subAgentPrompt = ""
	if m.subAgentChat != nil {
		m.subAgentChat.ClearMessages()
	}
	if m.workflowViewReturnSessionID != "" {
		m.workflowViewSessionID = m.workflowViewReturnSessionID
		m.workflowViewReturnSessionID = ""
	}
	m.updateLayoutAndSize()
}

// loadSubAgentMessages fetches a sub-agent session's message history
// asynchronously.
func (m *UI) loadSubAgentMessages(sessionID string) tea.Cmd {
	return func() tea.Msg {
		msgs, err := m.com.Workspace.ListMessages(context.Background(), sessionID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return subAgentMessagesLoadedMsg{sessionID: sessionID, messages: msgs}
	}
}

// buildSubAgentMessageItems converts a sub-agent's raw message history
// into renderable chat items. Unlike setSessionMessages, it does not
// recurse into further nested agent tool calls (a sub-agent spawning
// its own sub-agent chat is not supported) and skips the end-turn info
// footer, keeping the fullscreen view focused on raw activity.
func (m *UI) buildSubAgentMessageItems(msgs []message.Message) []chat.MessageItem {
	msgPtrs := make([]*message.Message, len(msgs))
	for i := range msgs {
		msgPtrs[i] = &msgs[i]
	}
	toolResultMap := chat.BuildToolResultMap(msgPtrs)
	items := make([]chat.MessageItem, 0, len(msgs)*2)
	for _, msg := range msgPtrs {
		items = append(items, chat.ExtractMessageItems(m.com.Styles, msg, toolResultMap)...)
	}
	return items
}

// handleSubAgentMessagesLoaded populates the fullscreen sub-agent chat
// once its message history has loaded.
func (m *UI) handleSubAgentMessagesLoaded(msg subAgentMessagesLoadedMsg) tea.Cmd {
	if msg.sessionID != m.subAgentSessionID {
		// Stale load from a view the user already left.
		return nil
	}
	var cmds []tea.Cmd
	items := m.buildSubAgentMessageItems(msg.messages)
	if cmd := m.startAnimClock(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := m.subAgentChat.SetMessages(items...); cmd != nil {
		cmds = append(cmds, cmd)
	}
	m.subAgentChat.SelectLast()
	if cmd := m.subAgentChat.ScrollToBottom(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Sequence(cmds...)
}

// appendSubAgentMessage appends a newly created message to the
// fullscreen sub-agent chat when it arrives for the currently viewed
// sub-agent session. Mirrors appendSessionMessage's role handling: a
// new Tool-role message carries results that get linked to their
// existing tool-call items rather than rendered as items of their own.
func (m *UI) appendSubAgentMessage(msg message.Message) tea.Cmd {
	var cmds []tea.Cmd

	switch msg.Role {
	case message.User, message.Assistant:
		if m.subAgentChat.MessageItem(msg.ID) != nil {
			return nil
		}
		items := chat.ExtractMessageItems(m.com.Styles, &msg, nil)
		if cmd := m.startAnimClock(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.subAgentChat.AppendMessages(items...)
	case message.Tool:
		for _, tr := range msg.ToolResults() {
			if toolItem, ok := m.subAgentChat.MessageItem(tr.ToolCallID).(chat.ToolMessageItem); ok {
				toolItem.SetResult(&tr)
			}
		}
	}

	if m.subAgentChat.Follow() {
		if cmd := m.subAgentChat.ScrollToBottom(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Sequence(cmds...)
}

// updateSubAgentMessage updates an existing message in the fullscreen
// sub-agent chat: assistant text/thinking as it streams in, and tool
// calls as they start and finish. Mirrors updateSessionMessage's
// logic (including removing an assistant placeholder that ends up
// with no visible content once it resolves to tool-calls-only) so a
// turn's "thinking" spinner always reaches a terminal state instead
// of animating forever and piling up across turns; it omits the
// end-turn info footer, keeping the fullscreen view focused on raw
// activity.
func (m *UI) updateSubAgentMessage(msg message.Message) tea.Cmd {
	var cmds []tea.Cmd
	existingItem := m.subAgentChat.MessageItem(msg.ID)

	if assistantItem, ok := existingItem.(*chat.AssistantMessageItem); ok {
		assistantItem.SetMessage(&msg)
	}

	shouldRenderAssistant := chat.ShouldRenderAssistantMessage(&msg)
	if !shouldRenderAssistant && len(msg.ToolCalls()) > 0 && existingItem != nil {
		m.subAgentChat.RemoveMessage(msg.ID)
	}

	var items []chat.MessageItem
	for _, tc := range msg.ToolCalls() {
		existingToolItem := m.subAgentChat.MessageItem(tc.ID)
		if toolItem, ok := existingToolItem.(chat.ToolMessageItem); ok {
			existingToolCall := toolItem.ToolCall()
			if (tc.Finished && !existingToolCall.Finished) || tc.Input != existingToolCall.Input {
				toolItem.SetToolCall(tc)
			}
		}
		if existingToolItem == nil {
			items = append(items, chat.NewToolMessageItem(m.com.Styles, msg.ID, tc, nil, false))
		}
	}
	if cmd := m.startAnimClock(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	m.subAgentChat.AppendMessages(items...)
	if m.subAgentChat.Follow() {
		if cmd := m.subAgentChat.ScrollToBottom(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Sequence(cmds...)
}

// sendToSubAgent steers the sub-agent currently focused in the
// fullscreen view by injecting a follow-up user prompt into its
// session.
func (m *UI) sendToSubAgent(content string) tea.Cmd {
	sessionID := m.subAgentSessionID
	return func() tea.Msg {
		if err := m.com.Workspace.AgentSendToSubAgent(context.Background(), sessionID, content); err != nil {
			return util.NewErrorMsg(err)
		}
		return nil
	}
}

// drawSubAgentBanner renders the fullscreen sub-agent view's header
// banner (task prompt) into the top of area, returning the remaining
// area for the chat content below it.
func (m *UI) drawSubAgentBanner(scr uv.Screen, area uv.Rectangle) uv.Rectangle {
	sty := m.com.Styles
	tagText := "Sub-agent"
	if m.workflowViewReturnSessionID != "" {
		tagText = "Workflow agent"
	}
	tag := sty.Tool.AgentTaskTag.Render(tagText)
	prompt := ansi.Truncate(m.subAgentPrompt, max(area.Dx()-lipgloss.Width(tag)-1, 0), "…")
	line := lipgloss.JoinHorizontal(lipgloss.Left, tag, " ", sty.Tool.AgentPrompt.Render(prompt))

	banner := uv.Rectangle(area)
	banner.Max.Y = min(banner.Min.Y+subAgentBannerHeight, area.Max.Y)
	uv.NewStyledString(line).Draw(scr, banner)

	rest := uv.Rectangle(area)
	rest.Min.Y = banner.Max.Y
	return rest
}

// splitMainPills carves the pills panel band off the bottom of
// mainRect, returning the remaining chat area and the pills rect
// (zero-height when the pills panel isn't shown).
func (m *UI) splitMainPills(mainRect image.Rectangle) (chatRect, pillsRect image.Rectangle) {
	pillsHeight := min(m.pillsAreaHeight(), mainRect.Dy())
	if pillsHeight > 0 {
		layout.Vertical(
			layout.Len(mainRect.Dy()-pillsHeight),
			layout.Fill(1),
		).Split(mainRect).Assign(&chatRect, &pillsRect)
		return chatRect, pillsRect
	}
	return mainRect, image.Rectangle{}
}

// splitEditorAgentList carves the editor (input box) and, below it,
// the agent picker list band off the bottom of mainRect. The agent
// list is zero-height (and thus hidden) when no sub-agents are
// running. Returns the remaining chat area, the editor rect, and the
// agent list rect, in that top-to-bottom visual order.
func (m *UI) splitEditorAgentList(mainRect image.Rectangle, editorHeight int) (chatAreaRect, editorRect, agentListRect image.Rectangle) {
	agentListHeight := min(m.agentListAreaHeight(), max(mainRect.Dy()-editorHeight, 0))

	layout.Vertical(
		layout.Len(mainRect.Dy()-editorHeight-agentListHeight),
		layout.Fill(1),
	).Split(mainRect).Assign(&chatAreaRect, &editorRect)

	if agentListHeight > 0 {
		layout.Vertical(
			layout.Len(editorHeight),
			layout.Fill(1),
		).Split(editorRect).Assign(&editorRect, &agentListRect)
	}

	return chatAreaRect, editorRect, agentListRect
}

// -----------------------------------------------------------------------------
// Agent picker list
// -----------------------------------------------------------------------------
//
// An always-visible list of entries — "Main" plus one row per running
// sub-agent — is shown directly below whichever chat is currently on
// screen whenever at least one sub-agent is running (mirroring Claude
// Code's teammate list). Pressing Down at the bottom of a chat moves
// focus into the list; Up/Down there move the selection; Enter opens
// the selected entry; Up on the top ("Main") entry (or Esc) returns
// focus to the chat.

// subAgentEntry describes a currently-running sub-agent (an unfinished
// "agent" tool call).
type subAgentEntry struct {
	SessionID  string
	ToolCallID string
	Prompt     string
}

// runningSubAgents returns the currently running (unfinished) "agent" and
// "agentic_fetch" tool calls in the main chat, in display order. Both
// dispatch through the same runSubAgent path (see coordinator.runSubAgent)
// and derive their child session ID the same way, so both are listed here;
// they only differ enough to need separate tool-call param types
// (agentic_fetch has no ResumeSessionID).
func (m *UI) runningSubAgents() []subAgentEntry {
	var entries []subAgentEntry
	for _, item := range m.chat.Items() {
		if entry, ok := m.subAgentEntryFor(item); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// subAgentEntryFor extracts a picker-list entry from a chat item if it is
// an unfinished "agent" or "agentic_fetch" tool call, or returns ok=false
// for anything else (including a finished one of either kind).
func (m *UI) subAgentEntryFor(item chat.MessageItem) (subAgentEntry, bool) {
	var (
		toolItem chat.ToolMessageItem
		prompt   string
		resumeID string
	)
	switch v := item.(type) {
	case *chat.AgentToolMessageItem:
		if v.Finished() {
			return subAgentEntry{}, false
		}
		toolItem = v
		var params agent.AgentParams
		_ = json.Unmarshal([]byte(v.ToolCall().Input), &params)
		prompt = params.Prompt
		// A resumed call reuses an existing session (see
		// AgentParams.ResumeSessionID) rather than the one derived from
		// this tool call's own message/tool-call ID, which would be
		// empty since no agent() dispatch created it.
		resumeID = params.ResumeSessionID
	case *chat.AgenticFetchToolMessageItem:
		if v.Finished() {
			return subAgentEntry{}, false
		}
		toolItem = v
		var params agenttools.AgenticFetchParams
		_ = json.Unmarshal([]byte(v.ToolCall().Input), &params)
		prompt = params.Prompt
		// agentic_fetch never resumes an existing session.
	default:
		return subAgentEntry{}, false
	}

	tc := toolItem.ToolCall()
	sessionID := resumeID
	if sessionID == "" {
		sessionID = m.com.Workspace.CreateAgentToolSessionID(toolItem.MessageID(), tc.ID)
	}
	return subAgentEntry{
		SessionID:  sessionID,
		ToolCallID: tc.ID,
		Prompt:     prompt,
	}, true
}

// agentListEntryKind distinguishes a plain sub-agent entry from a
// background workflow entry, and a scheduled task entry, in the
// picker list.
type agentListEntryKind int

const (
	agentListKindSubAgent agentListEntryKind = iota
	agentListKindWorkflow
	agentListKindSchedule
)

// agentListEntry is one row in the agent picker list.
type agentListEntry struct {
	Label      string
	SessionID  string // empty for "Main" and for schedule entries
	ToolCallID string
	// TaskID identifies a schedule entry (agentListKindSchedule); the
	// registry keys scheduled tasks by ID rather than session, since a
	// task's firings run in the origin session rather than a
	// dedicated one.
	TaskID string
	// Stopped is true for a schedule entry whose task already stopped
	// (canceled, expired, or reached max_runs), so selecting it
	// doesn't attempt a redundant cancel.
	Stopped bool
	Kind    agentListEntryKind
}

// agentListEntries returns the picker list's entries: "Main" first,
// then one entry per running sub-agent, one per background workflow,
// and one per scheduled task (ScheduleCron/ScheduleWakeup).
func (m *UI) agentListEntries() []agentListEntry {
	entries := make([]agentListEntry, 0, 1)
	entries = append(entries, agentListEntry{Label: "Main"})
	for _, sa := range m.runningSubAgents() {
		label := ansi.Truncate(sa.Prompt, maxAgentListPromptLength, "…")
		entries = append(entries, agentListEntry{
			Label:      label,
			SessionID:  sa.SessionID,
			ToolCallID: sa.ToolCallID,
			Kind:       agentListKindSubAgent,
		})
	}
	for _, wf := range m.runningWorkflows() {
		label := ansi.Truncate(workflowListLabel(wf), maxAgentListPromptLength, "…")
		entries = append(entries, agentListEntry{
			Label:      label,
			SessionID:  wf.SessionID,
			ToolCallID: wf.ToolCallID,
			Kind:       agentListKindWorkflow,
		})
	}
	for _, s := range m.runningSchedules() {
		label := ansi.Truncate(scheduleListLabel(s), maxAgentListPromptLength, "…")
		entries = append(entries, agentListEntry{
			Label:   label,
			TaskID:  s.ID,
			Stopped: s.State != agent.ScheduleActive,
			Kind:    agentListKindSchedule,
		})
	}
	return entries
}

// workflowListLabel builds the picker-row label for a background
// workflow, e.g. "◇ deep-research · what is X" with a state marker.
func workflowListLabel(wf agent.WorkflowStatus) string {
	marker := "◇"
	switch wf.State {
	case agent.WorkflowCompleted:
		marker = "✓"
	case agent.WorkflowFailed:
		marker = "✗"
	case agent.WorkflowCanceled:
		marker = "⊘"
	}
	label := marker + " " + wf.Name
	if wf.Args != "" {
		label += " · " + wf.Args
	}
	return label
}

// scheduleListLabel builds the picker-row label for a scheduled task,
// e.g. "◇ cron · check the build" or "⊘ wakeup · watch the deploy"
// with a state marker and its kind.
func scheduleListLabel(s agent.ScheduledTaskStatus) string {
	marker := "◇"
	if s.State == agent.ScheduleStopped {
		if s.StopReason == "canceled" {
			marker = "⊘"
		} else {
			marker = "✓"
		}
	}
	kind := "cron"
	if s.Kind == agent.ScheduleKindWakeup {
		kind = "wakeup"
	}
	return fmt.Sprintf("%s %s · %s", marker, kind, s.Prompt)
}

// agentListAreaHeight returns the number of rows the agent picker list
// needs (including a one-row gap above it, separating it from the
// editor), or 0 if no sub-agents are currently running (in which case
// the list is hidden entirely).
func (m *UI) agentListAreaHeight() int {
	if !m.hasSession() {
		return 0
	}
	if len(m.runningSubAgents()) == 0 && len(m.runningWorkflows()) == 0 && len(m.runningSchedules()) == 0 {
		return 0
	}
	return len(m.agentListEntries()) + 1
}

// runningWorkflows returns the coordinator's background workflows,
// guarding against a nil Workspace (some tests construct a bare UI).
func (m *UI) runningWorkflows() []agent.WorkflowStatus {
	if m.com == nil || m.com.Workspace == nil {
		return nil
	}
	return m.com.Workspace.AgentRunningWorkflows()
}

// runningSchedules returns the coordinator's scheduled tasks
// (ScheduleCron/ScheduleWakeup), guarding against a nil Workspace
// (some tests construct a bare UI).
func (m *UI) runningSchedules() []agent.ScheduledTaskStatus {
	if m.com == nil || m.com.Workspace == nil {
		return nil
	}
	return m.com.Workspace.AgentRunningSchedules()
}

// currentAgentListIndex returns the index into agentListEntries() of
// the entry currently being viewed (0 for Main, or the matching
// sub-agent).
func (m *UI) currentAgentListIndex(entries []agentListEntry) int {
	if m.subAgentSessionID == "" {
		return 0
	}
	for i, e := range entries {
		if e.SessionID == m.subAgentSessionID {
			return i
		}
	}
	return 0
}

// enterAgentList moves keyboard focus from the editor into the agent
// picker list, defaulting the selection to whichever entry is
// currently being viewed. It is the sole entry point into the list
// (see the Down-arrow handling in Update's uiFocusEditor branch),
// mirroring Claude Code's "press Down in the input box" flow.
func (m *UI) enterAgentList() {
	entries := m.agentListEntries()
	if len(entries) == 0 {
		return
	}
	m.textarea.Blur()
	m.focus = uiFocusMain
	m.agentListFocused = true
	m.agentListSelected = m.currentAgentListIndex(entries)
}

// exitAgentListToEditor returns keyboard focus to the prompt input,
// mirroring how the list was entered.
func (m *UI) exitAgentListToEditor() {
	m.agentListFocused = false
	m.focus = uiFocusEditor
	m.textarea.Focus()
}

// handleAgentListKeyMsg handles keyboard input while the agent picker
// list has focus.
func (m *UI) handleAgentListKeyMsg(msg tea.KeyMsg) tea.Cmd {
	entries := m.agentListEntries()
	if len(entries) == 0 {
		m.exitAgentListToEditor()
		return nil
	}
	switch {
	case key.Matches(msg, m.keyMap.Chat.Up):
		if m.agentListSelected == 0 {
			m.exitAgentListToEditor()
			return nil
		}
		m.agentListSelected--
	case key.Matches(msg, m.keyMap.Chat.Down):
		if m.agentListSelected < len(entries)-1 {
			m.agentListSelected++
		}
	case key.Matches(msg, m.keyMap.Chat.Confirm):
		return m.confirmAgentListSelection()
	}
	return nil
}

// confirmAgentListSelection switches the view to whichever entry is
// highlighted in the agent picker list, then returns focus to the
// prompt input so the user can immediately type a follow-up.
func (m *UI) confirmAgentListSelection() tea.Cmd {
	entries := m.agentListEntries()
	if len(entries) == 0 {
		m.exitAgentListToEditor()
		return nil
	}
	idx := max(0, min(len(entries)-1, m.agentListSelected))
	entry := entries[idx]
	m.exitAgentListToEditor()
	if entry.Kind == agentListKindSchedule {
		if entry.Stopped {
			return util.ReportInfo(fmt.Sprintf("%s is already stopped.", entry.TaskID))
		}
		return m.openScheduleCancelDialog(entry.TaskID)
	}
	if entry.SessionID == "" {
		// "Main" selected.
		if m.subAgentSessionID != "" {
			m.exitSubAgentView()
		}
		return nil
	}
	if entry.Kind == agentListKindWorkflow {
		return m.enterWorkflowView(entry.SessionID)
	}
	if entry.SessionID == m.subAgentSessionID {
		// Already viewing this sub-agent.
		return nil
	}
	// Find the matching sub-agent's prompt for the banner.
	prompt := ""
	for _, sa := range m.runningSubAgents() {
		if sa.SessionID == entry.SessionID {
			prompt = sa.Prompt
			break
		}
	}
	return m.enterSubAgentView(entry.SessionID, entry.ToolCallID, prompt)
}

// renderAgentList renders the agent picker list.
func (m *UI) renderAgentList(width int) string {
	entries := m.agentListEntries()
	if len(entries) == 0 {
		return ""
	}
	sty := m.com.Styles
	currentIdx := m.currentAgentListIndex(entries)

	lines := []string{""} // Gap separating the list from the editor above.
	for i, e := range entries {
		label := e.Label
		if ansi.StringWidth(label) > width-2 && width > 2 {
			label = ansi.Truncate(label, width-3, "…")
		}
		prefix := sty.Pills.QueueItemPrefix.Render() + " "
		text := sty.Pills.QueueItemText
		if i == currentIdx {
			text = text.Bold(true)
		}
		if m.agentListFocused && i == m.agentListSelected {
			prefix = "▸ "
			text = text.Bold(true)
		}
		lines = append(lines, prefix+text.Render(label))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}
