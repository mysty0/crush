package model

import (
	"context"
	"fmt"
	"image"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/charmbracelet/x/ansi"
)

// enterWorkflowView switches the main content area to the two-pane
// workflow view for the given workflow session: phases on the left,
// the selected phase's agents on the right.
func (m *UI) enterWorkflowView(workflowSessionID string) tea.Cmd {
	m.workflowViewSessionID = workflowSessionID
	m.workflowViewSelectedPhase = 0
	m.workflowViewRightFocus = false
	m.workflowViewAgentScroll = 0
	m.updateLayoutAndSize()
	return nil
}

// exitWorkflowView returns the main content area to the normal chat.
func (m *UI) exitWorkflowView() {
	m.workflowViewSessionID = ""
	m.workflowViewRightFocus = false
	m.updateLayoutAndSize()
}

// enterWorkflowAgentView switches the main content area from the
// workflow two-pane view into a live, read-only transcript of one of
// its dispatched sub-agents. Esc returns to the two-pane view instead
// of canceling the sub-agent (see the Cancel-key handling in ui.go),
// since drilling into a workflow agent is inspection, not a steerable
// session.
func (m *UI) enterWorkflowAgentView(a agent.WorkflowAgentStatus) tea.Cmd {
	m.workflowViewReturnSessionID = m.workflowViewSessionID
	m.workflowViewSessionID = ""
	label := a.Label
	if label == "" {
		label = "agent"
	}
	if a.Phase != "" {
		label = a.Phase + " · " + label
	}
	if modelName := m.workflowAgentModelName(a); modelName != "" {
		label += " (" + modelName + ")"
	}
	return m.enterSubAgentView(a.SessionID, "", label)
}

// handleWorkflowViewKeyMsg handles navigation while the workflow view
// is active: Up/Down move the phase selection (left pane) or the agent
// selection (right pane), Left/Right/Tab switch pane focus, Confirm on
// the right pane opens the selected agent's live transcript, "c"
// cancels the workflow, and Esc/Cancel exits.
func (m *UI) handleWorkflowViewKeyMsg(msg tea.KeyMsg) tea.Cmd {
	status, ok := m.com.Workspace.AgentWorkflowStatus(m.workflowViewSessionID)
	if !ok {
		m.exitWorkflowView()
		return nil
	}
	phases := sortedWorkflowPhases(status)
	var phaseName string
	if m.workflowViewSelectedPhase < len(phases) {
		phaseName = phases[m.workflowViewSelectedPhase].Name
	}
	agents := agentsForPhase(status, phaseName)

	switch {
	case key.Matches(msg, m.keyMap.Chat.Cancel):
		m.exitWorkflowView()
		return nil
	case msg.String() == "c":
		m.com.Workspace.AgentCancelWorkflow(m.workflowViewSessionID)
		return nil
	case msg.String() == "tab", key.Matches(msg, m.keyMap.Chat.PillRight):
		m.workflowViewRightFocus = true
	case key.Matches(msg, m.keyMap.Chat.PillLeft):
		m.workflowViewRightFocus = false
	case key.Matches(msg, m.keyMap.Chat.Confirm):
		if m.workflowViewRightFocus && m.workflowViewSelectedAgent < len(agents) {
			return m.enterWorkflowAgentView(agents[m.workflowViewSelectedAgent])
		}
	case key.Matches(msg, m.keyMap.Chat.Up):
		if m.workflowViewRightFocus {
			if m.workflowViewSelectedAgent > 0 {
				m.workflowViewSelectedAgent--
			}
		} else if m.workflowViewSelectedPhase > 0 {
			m.workflowViewSelectedPhase--
			m.workflowViewSelectedAgent = 0
			m.workflowViewAgentScroll = 0
		}
	case key.Matches(msg, m.keyMap.Chat.Down):
		if m.workflowViewRightFocus {
			if m.workflowViewSelectedAgent < len(agents)-1 {
				m.workflowViewSelectedAgent++
			}
		} else if m.workflowViewSelectedPhase < len(phases)-1 {
			m.workflowViewSelectedPhase++
			m.workflowViewSelectedAgent = 0
			m.workflowViewAgentScroll = 0
		}
	}
	return nil
}

// drawWorkflowView renders the two-pane workflow view into area.
func (m *UI) drawWorkflowView(scr uv.Screen, area image.Rectangle) {
	status, ok := m.com.Workspace.AgentWorkflowStatus(m.workflowViewSessionID)
	if !ok {
		// The workflow was cleared from the registry; fall back to the
		// normal chat on the next frame.
		m.chat.Draw(scr, area)
		return
	}

	sty := m.com.Styles
	rect := uv.Rectangle(area)

	// Header banner.
	tag := sty.Tool.AgentTaskTag.Render("Workflow")
	header := lipgloss.JoinHorizontal(
		lipgloss.Left,
		tag, " ",
		sty.Tool.AgentPrompt.Render(ansi.Truncate(workflowViewTitle(status), max(area.Dx()-lipgloss.Width(tag)-1, 0), "…")),
	)
	headerRect := rect
	headerRect.Max.Y = min(headerRect.Min.Y+1, rect.Max.Y)
	uv.NewStyledString(header).Draw(scr, headerRect)

	// Footer hint.
	hint := lipgloss.NewStyle().Faint(true).Render("↑/↓ navigate · ←/→ switch pane · enter view agent · c cancel · esc back")
	footerRect := rect
	footerRect.Min.Y = max(rect.Max.Y-1, rect.Min.Y)
	uv.NewStyledString(hint).Draw(scr, footerRect)

	// Body between header and footer, split into two panes.
	body := rect
	body.Min.Y = headerRect.Max.Y
	body.Max.Y = footerRect.Min.Y
	if body.Dy() <= 0 {
		return
	}

	leftWidth := max(24, body.Dx()/3)
	var leftRect, rightRect image.Rectangle
	layout.Horizontal(
		layout.Len(leftWidth),
		layout.Fill(1),
	).Split(image.Rectangle(body)).Assign(&leftRect, &rightRect)

	phases := sortedWorkflowPhases(status)
	if m.workflowViewSelectedPhase >= len(phases) {
		m.workflowViewSelectedPhase = max(0, len(phases)-1)
	}

	m.drawWorkflowPhases(scr, uv.Rectangle(leftRect), status, phases)
	m.drawWorkflowAgents(scr, uv.Rectangle(rightRect), status, phases)
}

// drawWorkflowPhases renders the left pane: one row per phase with a
// status glyph and agent count, current selection highlighted.
func (m *UI) drawWorkflowPhases(scr uv.Screen, area uv.Rectangle, status agent.WorkflowStatus, phases []agent.WorkflowPhaseStatus) {
	var lines []string
	title := lipgloss.NewStyle().Bold(true).Render("Phases")
	lines = append(lines, title, "")

	for i, ph := range phases {
		glyph := workflowPhaseGlyph(ph, status.State)
		label := fmt.Sprintf("%s %s", glyph, ph.Name)
		if ph.AgentCount > 0 {
			label += fmt.Sprintf("  (%d)", ph.AgentCount)
		}
		style := lipgloss.NewStyle()
		if i == m.workflowViewSelectedPhase && !m.workflowViewRightFocus {
			style = style.Bold(true).Reverse(true)
		} else if i == m.workflowViewSelectedPhase {
			style = style.Bold(true)
		}
		lines = append(lines, style.Render(ansi.Truncate(label, max(area.Dx()-1, 0), "…")))
	}

	uv.NewStyledString(strings.Join(lines, "\n")).Draw(scr, area)
}

// drawWorkflowAgents renders the right pane: one row per agent in the
// selected phase, with time, tokens, and tool-call stats.
func (m *UI) drawWorkflowAgents(scr uv.Screen, area uv.Rectangle, status agent.WorkflowStatus, phases []agent.WorkflowPhaseStatus) {
	var phaseName string
	if m.workflowViewSelectedPhase < len(phases) {
		phaseName = phases[m.workflowViewSelectedPhase].Name
	}

	var lines []string
	headerStyle := lipgloss.NewStyle().Bold(true)
	if m.workflowViewRightFocus {
		headerStyle = headerStyle.Reverse(true)
	}
	lines = append(lines, headerStyle.Render("Agents"), "")

	agents := agentsForPhase(status, phaseName)
	if len(agents) == 0 {
		lines = append(lines, lipgloss.NewStyle().Faint(true).Render("No agents in this phase yet."))
		uv.NewStyledString(strings.Join(lines, "\n")).Draw(scr, area)
		return
	}

	// Clamp the selection, then keep it within the visible window,
	// scrolling as needed.
	if m.workflowViewSelectedAgent > len(agents)-1 {
		m.workflowViewSelectedAgent = max(0, len(agents)-1)
	}
	visible := max(1, area.Dy()-2)
	if m.workflowViewAgentScroll > m.workflowViewSelectedAgent {
		m.workflowViewAgentScroll = m.workflowViewSelectedAgent
	}
	if m.workflowViewSelectedAgent >= m.workflowViewAgentScroll+visible {
		m.workflowViewAgentScroll = m.workflowViewSelectedAgent - visible + 1
	}
	if m.workflowViewAgentScroll > len(agents)-1 {
		m.workflowViewAgentScroll = max(0, len(agents)-1)
	}
	start := m.workflowViewAgentScroll
	end := min(len(agents), start+visible)

	for i, a := range agents[start:end] {
		idx := start + i
		selected := m.workflowViewRightFocus && idx == m.workflowViewSelectedAgent
		lines = append(lines, m.renderWorkflowAgentRow(a, area.Dx()-1, selected))
	}

	uv.NewStyledString(strings.Join(lines, "\n")).Draw(scr, area)
}

// renderWorkflowAgentRow builds one agent row: "glyph label  time  tok
// calls". The row is bolded when selected.
func (m *UI) renderWorkflowAgentRow(a agent.WorkflowAgentStatus, width int, selected bool) string {
	glyph := "◐"
	if a.Done {
		glyph = "●"
	}

	elapsed := time.Since(a.StartedAt).Truncate(time.Millisecond)
	tokens, calls := m.workflowAgentStats(a.SessionID)

	label := a.Label
	if label == "" {
		label = "agent"
	}

	var stats string
	if modelName := m.workflowAgentModelName(a); modelName != "" {
		stats = fmt.Sprintf("%s  %s  %s tok  %d calls", modelName, formatDuration(elapsed), formatTokens(tokens), calls)
	} else {
		stats = fmt.Sprintf("%s  %s tok  %d calls", formatDuration(elapsed), formatTokens(tokens), calls)
	}
	// Reserve space for the stats column on the right.
	statsWidth := lipgloss.Width(stats)
	labelWidth := max(0, width-statsWidth-2-lipgloss.Width(glyph)-1)
	labelText := ansi.Truncate(label, labelWidth, "…")

	left := fmt.Sprintf("%s %s", glyph, labelText)
	pad := max(1, width-lipgloss.Width(left)-statsWidth)
	if selected {
		left = lipgloss.NewStyle().Bold(true).Render(left)
	}
	return left + strings.Repeat(" ", pad) + lipgloss.NewStyle().Faint(true).Render(stats)
}

// workflowAgentModelName resolves the display name of the model an
// agent ran on, falling back to the raw model ID, or "" if the
// registry has no model recorded (e.g. from before this field
// existed).
func (m *UI) workflowAgentModelName(a agent.WorkflowAgentStatus) string {
	if a.Model == "" {
		return ""
	}
	if model := m.com.Config().GetModel(a.Provider, a.Model); model != nil {
		return model.Name
	}
	return a.Model
}

// workflowAgentStats returns the total tokens and tool-call count for a
// workflow sub-agent's session, computed from its persisted session and
// message history.
func (m *UI) workflowAgentStats(sessionID string) (tokens int64, toolCalls int) {
	sess, err := m.com.Workspace.GetSession(context.Background(), sessionID)
	if err == nil {
		tokens = sess.PromptTokens + sess.CompletionTokens
	}
	msgs, err := m.com.Workspace.ListMessages(context.Background(), sessionID)
	if err == nil {
		for i := range msgs {
			toolCalls += len(msgs[i].ToolCalls())
		}
	}
	return tokens, toolCalls
}

// --- helpers ---

func workflowViewTitle(status agent.WorkflowStatus) string {
	title := status.Name
	if status.Args != "" {
		title += ": " + status.Args
	}
	switch status.State {
	case agent.WorkflowCompleted:
		title += "  (completed)"
	case agent.WorkflowFailed:
		title += "  (failed)"
	case agent.WorkflowCanceled:
		title += "  (canceled)"
	}
	return title
}

// sortedWorkflowPhases returns the workflow's phases in first-seen
// order.
func sortedWorkflowPhases(status agent.WorkflowStatus) []agent.WorkflowPhaseStatus {
	phases := make([]agent.WorkflowPhaseStatus, len(status.Phases))
	copy(phases, status.Phases)
	// Phases are already appended in Order; sort defensively by Order.
	for i := 1; i < len(phases); i++ {
		for j := i; j > 0 && phases[j].Order < phases[j-1].Order; j-- {
			phases[j], phases[j-1] = phases[j-1], phases[j]
		}
	}
	return phases
}

// agentsForPhase returns the agents belonging to the named phase, in
// dispatch order.
func agentsForPhase(status agent.WorkflowStatus, phase string) []agent.WorkflowAgentStatus {
	var out []agent.WorkflowAgentStatus
	for _, a := range status.Agents {
		if a.Phase == phase {
			out = append(out, a)
		}
	}
	return out
}

// workflowPhaseGlyph picks a status glyph for a phase: done, active, or
// pending.
func workflowPhaseGlyph(ph agent.WorkflowPhaseStatus, state agent.WorkflowRunState) string {
	switch {
	case ph.Active && state == agent.WorkflowRunning:
		return "◐"
	case ph.AgentCount > 0:
		return "●"
	default:
		return "○"
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
