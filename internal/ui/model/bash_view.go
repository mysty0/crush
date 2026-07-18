package model

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// bashViewTickInterval is how often the fullscreen bash output view
// re-fetches a background job's stdout/stderr while it is open.
const bashViewTickInterval = 500 * time.Millisecond

// bashViewPageStep is the number of lines Up/Down page keys move the
// scroll offset by in the fullscreen bash output view.
const bashViewPageStep = 10

// enterBashOutputView switches the main content area to a fullscreen,
// live-updating view of a background bash job's stdout/stderr,
// identified by jobID (see agent.TaskKindBash / TaskRef.ID). It kicks
// off the first output fetch immediately; handleBashViewOutput
// reschedules the next one on a tea.Tick as long as the view stays
// open (see bashViewTickInterval).
func (m *UI) enterBashOutputView(jobID string) tea.Cmd {
	m.bashViewJobID = jobID
	m.bashViewStdout = ""
	m.bashViewStderr = ""
	m.bashViewDone = false
	m.bashViewOK = true
	m.bashViewScroll = 0
	m.bashViewFollow = true
	m.bashViewSeq++
	m.updateLayoutAndSize()
	return m.fetchBashOutputCmd(jobID, m.bashViewSeq)
}

// exitBashOutputView returns the main content area to the normal
// chat. Bumping bashViewSeq (via clearing bashViewJobID, checked by
// handleBashViewOutput/handleBashViewTick) is what stops the
// in-flight fetch/tick loop from rescheduling itself further.
func (m *UI) exitBashOutputView() {
	m.bashViewJobID = ""
	m.updateLayoutAndSize()
}

// bashViewTickMsg drives the next output fetch for the fullscreen bash
// output view. seq is checked against m.bashViewSeq on arrival so a
// leftover tick from a view the user already left (or from viewing a
// different job) is dropped rather than starting a second, duplicate
// fetch/tick chain.
type bashViewTickMsg struct {
	seq uint64
}

// bashViewOutputMsg carries a freshly fetched background job output
// snapshot for the fullscreen bash output view.
type bashViewOutputMsg struct {
	seq    uint64
	stdout string
	stderr string
	done   bool
	ok     bool
}

// bashOutputTickCmd schedules the next output fetch after
// bashViewTickInterval for the given view generation.
func bashOutputTickCmd(seq uint64) tea.Cmd {
	return tea.Tick(bashViewTickInterval, func(time.Time) tea.Msg {
		return bashViewTickMsg{seq: seq}
	})
}

// fetchBashOutputCmd asynchronously fetches a background job's
// current output. This is the only place AgentBackgroundJobOutput is
// called from -- never directly in Update -- since it can be called
// repeatedly while the view is open.
func (m *UI) fetchBashOutputCmd(jobID string, seq uint64) tea.Cmd {
	return func() tea.Msg {
		stdout, stderr, done, ok := m.com.Workspace.AgentBackgroundJobOutput(jobID)
		return bashViewOutputMsg{seq: seq, stdout: stdout, stderr: stderr, done: done, ok: ok}
	}
}

// handleBashViewTick handles one tick of the fullscreen bash output
// view's refresh loop, kicking off the next fetch. A stale tick (from
// an abandoned view generation) is dropped instead of fetching, which
// is what lets the loop stop itself once the view is closed.
func (m *UI) handleBashViewTick(msg bashViewTickMsg) tea.Cmd {
	if msg.seq != m.bashViewSeq || m.bashViewJobID == "" {
		return nil
	}
	return m.fetchBashOutputCmd(m.bashViewJobID, msg.seq)
}

// handleBashViewOutput applies a freshly fetched output snapshot to
// the fullscreen bash output view and schedules the next refresh
// tick. A stale result (seq mismatch, or the view was closed while
// the fetch was in flight) is dropped and the loop is not
// rescheduled, so it naturally stops once the view is closed.
func (m *UI) handleBashViewOutput(msg bashViewOutputMsg) tea.Cmd {
	if msg.seq != m.bashViewSeq || m.bashViewJobID == "" {
		return nil
	}
	m.bashViewStdout = msg.stdout
	m.bashViewStderr = msg.stderr
	m.bashViewDone = msg.done
	m.bashViewOK = msg.ok
	return bashOutputTickCmd(msg.seq)
}

// handleBashOutputViewKeyMsg handles navigation while the fullscreen
// bash output view is active: Up/Down and PageUp/PageDown scroll the
// output, and Esc/Cancel exits back to Main. Scrolling up disengages
// auto-follow (see drawBashOutputView); scrolling back down to the
// bottom re-engages it.
func (m *UI) handleBashOutputViewKeyMsg(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, m.keyMap.Chat.Cancel):
		m.exitBashOutputView()
	case key.Matches(msg, m.keyMap.Chat.Up):
		m.bashViewFollow = false
		m.bashViewScroll = max(0, m.bashViewScroll-1)
	case key.Matches(msg, m.keyMap.Chat.Down):
		m.bashViewScroll++
	case key.Matches(msg, m.keyMap.Chat.PageUp):
		m.bashViewFollow = false
		m.bashViewScroll = max(0, m.bashViewScroll-bashViewPageStep)
	case key.Matches(msg, m.keyMap.Chat.PageDown):
		m.bashViewScroll += bashViewPageStep
	}
	return nil
}

// drawBashOutputView renders the fullscreen bash output view into
// area: a header banner (job id + state marker), a scrollable body of
// the job's stdout (and stderr, clearly separated, if any), and a key
// hint footer.
func (m *UI) drawBashOutputView(scr uv.Screen, area uv.Rectangle) {
	rect := area

	headerRect := rect
	headerRect.Max.Y = min(headerRect.Min.Y+1, rect.Max.Y)
	m.drawBashOutputHeader(scr, headerRect)

	hint := lipgloss.NewStyle().Faint(true).Render("↑/↓ scroll · b/f page · esc back")
	footerRect := rect
	footerRect.Min.Y = max(rect.Max.Y-1, rect.Min.Y)
	uv.NewStyledString(hint).Draw(scr, footerRect)

	body := rect
	body.Min.Y = headerRect.Max.Y
	body.Max.Y = footerRect.Min.Y
	if body.Dy() <= 0 || body.Dx() <= 0 {
		return
	}

	lines := m.bashOutputLines(body.Dx())
	visible := body.Dy()
	maxScroll := max(0, len(lines)-visible)
	if m.bashViewFollow {
		m.bashViewScroll = maxScroll
	} else {
		m.bashViewScroll = max(0, min(m.bashViewScroll, maxScroll))
		if m.bashViewScroll >= maxScroll {
			m.bashViewFollow = true
		}
	}

	start := m.bashViewScroll
	end := min(len(lines), start+visible)
	uv.NewStyledString(strings.Join(lines[start:end], "\n")).Draw(scr, body)
}

// drawBashOutputHeader renders the job-id/state banner at the top of
// the fullscreen bash output view, mirroring the "Job PID ..." header
// convention used for bash tool calls in chat/bash.go.
func (m *UI) drawBashOutputHeader(scr uv.Screen, area uv.Rectangle) {
	sty := m.com.Styles
	state, label, found := m.bashViewTaskState()

	icon := bashViewStateIcon(sty, state)
	jobPart := sty.Tool.JobToolName.Render("Job")
	pidPart := sty.Tool.JobPID.Render("PID " + m.bashViewJobID)
	stateText := lipgloss.NewStyle().Faint(true).Render("(" + bashViewStateLabel(state, found) + ")")

	prefix := fmt.Sprintf("%s %s %s %s", icon, jobPart, pidPart, stateText)
	line := prefix
	if label != "" {
		prefixWidth := lipgloss.Width(prefix)
		available := area.Dx() - prefixWidth - 1
		if available >= 10 {
			line = prefix + " " + sty.Tool.JobDescription.Render(ansi.Truncate(label, available, "…"))
		}
	}
	uv.NewStyledString(line).Draw(scr, area)
}

// bashOutputLines builds the fullscreen bash output view's body as
// one styled line per output line, wrapping neither stdout nor
// stderr (matching the tail-scroll convention used for the in-chat
// ShellItem) but truncating each line to width so long lines don't
// corrupt layout. stdout is shown first, then stderr (if non-empty)
// under a clearly separated "stderr" label.
func (m *UI) bashOutputLines(width int) []string {
	sty := m.com.Styles
	var lines []string

	if !m.bashViewOK {
		lines = append(lines, sty.Tool.ErrorMessage.Render("Job output is no longer available."))
	}

	if stdout := strings.TrimRight(m.bashViewStdout, "\n"); stdout != "" {
		for _, ln := range strings.Split(stdout, "\n") {
			lines = append(lines, sty.Messages.ShellOutput.Render(ansi.Truncate(ln, width, "…")))
		}
	}

	if stderr := strings.TrimRight(m.bashViewStderr, "\n"); stderr != "" {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, lipgloss.NewStyle().Bold(true).Render("stderr"))
		for _, ln := range strings.Split(stderr, "\n") {
			lines = append(lines, sty.Tool.ErrorMessage.Render(ansi.Truncate(ln, width, "…")))
		}
	}

	if len(lines) == 0 {
		lines = append(lines, lipgloss.NewStyle().Faint(true).Render("No output yet."))
	}
	return lines
}

// bashViewTaskState looks up the currently viewed job's unified task
// state and label from the picker's own task list (agent.TaskStatus),
// the same source agentListEntries uses to build the "◇/✓/✗ bash ·
// ..." picker row. found is false once the job has aged out of that
// list (e.g. a long-finished job outside the picker's session scope),
// in which case the header falls back to the last known output
// snapshot's done/ok flags instead.
func (m *UI) bashViewTaskState() (state agent.TaskState, label string, found bool) {
	for _, t := range m.backgroundTasks() {
		if t.Ref.Kind == agent.TaskKindBash && t.Ref.ID == m.bashViewJobID {
			return t.State, t.Label, true
		}
	}
	if !m.bashViewOK {
		return agent.TaskStopped, "", false
	}
	if m.bashViewDone {
		return agent.TaskDone, "", false
	}
	return agent.TaskRunning, "", false
}

// bashViewStateIcon picks the tool-call style icon matching a
// background job's state, reusing the same icon set bash tool calls
// use in chat/bash.go so a job looks the same in the picker, the
// chat, and this fullscreen view.
func bashViewStateIcon(sty *styles.Styles, state agent.TaskState) string {
	switch state {
	case agent.TaskDone:
		return sty.Tool.IconSuccess.String()
	case agent.TaskFailed:
		return sty.Tool.IconError.String()
	case agent.TaskStopped:
		return sty.Tool.IconCancelled.String()
	default:
		return sty.Tool.IconPending.String()
	}
}

// bashViewStateLabel renders a short human label for a background
// job's state, e.g. "running", "done", "failed".
func bashViewStateLabel(state agent.TaskState, found bool) string {
	switch state {
	case agent.TaskDone:
		return "done"
	case agent.TaskFailed:
		return "failed"
	case agent.TaskStopped:
		if found {
			return "stopped"
		}
		return "unavailable"
	default:
		return "running"
	}
}
