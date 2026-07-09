package dialog

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

// ScheduleCancelID is the identifier for the scheduled-task cancel
// confirmation dialog.
const ScheduleCancelID = "schedule_cancel"

// ScheduleCancel is a confirmation dialog for stopping a scheduled task
// (created via ScheduleCron or ScheduleWakeup). It shows the task's
// metadata (kind, prompt, timing, run count) so the user knows exactly
// what they're about to stop before confirming — canceling a
// background loop can't be undone.
type ScheduleCancel struct {
	com  *common.Common
	task agent.ScheduledTaskStatus

	selectedNo bool // true if "No" button is selected
	keyMap     struct {
		LeftRight,
		EnterSpace,
		Yes,
		No,
		Tab,
		Close key.Binding
	}
}

var _ Dialog = (*ScheduleCancel)(nil)

// NewScheduleCancel creates a confirmation dialog for stopping task.
func NewScheduleCancel(com *common.Common, task agent.ScheduledTaskStatus) *ScheduleCancel {
	d := &ScheduleCancel{
		com:        com,
		task:       task,
		selectedNo: true,
	}
	d.keyMap.LeftRight = key.NewBinding(
		key.WithKeys("left", "right"),
		key.WithHelp("←/→", "switch options"),
	)
	d.keyMap.EnterSpace = key.NewBinding(
		key.WithKeys("enter", " "),
		key.WithHelp("enter/space", "confirm"),
	)
	d.keyMap.Yes = key.NewBinding(
		key.WithKeys("y", "Y"),
		key.WithHelp("y/Y", "yes"),
	)
	d.keyMap.No = key.NewBinding(
		key.WithKeys("n", "N"),
		key.WithHelp("n/N", "no"),
	)
	d.keyMap.Tab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch options"),
	)
	d.keyMap.Close = CloseKey
	return d
}

// ID implements [Dialog].
func (*ScheduleCancel) ID() string {
	return ScheduleCancelID
}

// HandleMsg implements [Dialog].
func (d *ScheduleCancel) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.LeftRight, d.keyMap.Tab):
			d.selectedNo = !d.selectedNo
		case key.Matches(msg, d.keyMap.EnterSpace):
			if !d.selectedNo {
				return ActionScheduleCancelConfirm{TaskID: d.task.ID}
			}
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Yes):
			return ActionScheduleCancelConfirm{TaskID: d.task.ID}
		case key.Matches(msg, d.keyMap.No):
			return ActionClose{}
		}
	}

	return nil
}

// Draw implements [Dialog].
func (d *ScheduleCancel) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	baseStyle := d.com.Styles.Dialog.Quit.Content
	labelStyle := lipgloss.NewStyle().Bold(true)
	faintStyle := lipgloss.NewStyle().Faint(true)

	kind := "Recurring (cron)"
	if d.task.Kind == agent.ScheduleKindWakeup {
		kind = "Self-paced (wakeup)"
	}

	var timing string
	if d.task.State == agent.ScheduleActive {
		timing = "Next fire: " + formatFireIn(d.task.NextFireAt)
	} else {
		timing = "Already stopped: " + d.task.StopReason
	}

	runs := fmt.Sprintf("%d", d.task.RunCount)
	if d.task.MaxRuns > 0 {
		runs += fmt.Sprintf(" / %d", d.task.MaxRuns)
	}

	prompt := d.task.Prompt
	if len(prompt) > scheduleCancelDialogMaxWidth-4 {
		prompt = prompt[:scheduleCancelDialogMaxWidth-7] + "..."
	}

	meta := lipgloss.JoinVertical(
		lipgloss.Left,
		labelStyle.Render(kind),
		faintStyle.Render(prompt),
		"",
		faintStyle.Render(timing),
		faintStyle.Render("Runs: "+runs),
	)

	question := "Stop this scheduled task?"
	buttonOpts := []common.ButtonOpts{
		{Text: "Yep!", Selected: !d.selectedNo, Padding: 3},
		{Text: "Nope", Selected: d.selectedNo, Padding: 3},
	}
	buttons := common.ButtonGroup(d.com.Styles, buttonOpts, " ")
	content := baseStyle.Render(
		lipgloss.JoinVertical(
			lipgloss.Center,
			meta,
			"",
			question,
			"",
			buttons,
		),
	)

	view := d.com.Styles.Dialog.Quit.Frame.Render(content)
	DrawCenter(scr, area, view)
	return nil
}

// ShortHelp implements [help.KeyMap].
func (d *ScheduleCancel) ShortHelp() []key.Binding {
	return []key.Binding{
		d.keyMap.LeftRight,
		d.keyMap.EnterSpace,
	}
}

// FullHelp implements [help.KeyMap].
func (d *ScheduleCancel) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{d.keyMap.LeftRight, d.keyMap.EnterSpace, d.keyMap.Yes, d.keyMap.No},
		{d.keyMap.Tab, d.keyMap.Close},
	}
}

// scheduleCancelDialogMaxWidth caps how much of a task's prompt is shown
// before truncating.
const scheduleCancelDialogMaxWidth = 56

// formatFireIn renders a future time as a short "in 5m" style string,
// mirroring relativeTime's past-tense sibling above.
func formatFireIn(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "any moment"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("in %ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("in %dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
