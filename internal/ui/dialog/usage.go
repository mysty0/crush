package dialog

import (
	"context"
	"fmt"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

// UsageID is the identifier for the plan usage dialog.
const (
	UsageID             = "usage"
	usageDialogMaxWidth = 60
)

// usageLoadedMsg carries the result of the asynchronous plan usage fetch.
type usageLoadedMsg struct {
	usage *claudecode.Utilization
	err   error
}

// Usage is a read-only dialog that shows the Claude Code subscription plan
// usage limits. It is only meaningful when the active provider is the
// claude-code subscription provider.
type Usage struct {
	com     *common.Common
	spinner spinner.Model
	loading bool

	usage *claudecode.Utilization
	err   error

	keyMap struct {
		Close key.Binding
	}
}

var (
	_ Dialog        = (*Usage)(nil)
	_ LoadingDialog = (*Usage)(nil)
)

// NewUsage creates a new plan usage dialog in its loading state.
func NewUsage(com *common.Common) *Usage {
	u := &Usage{com: com, loading: true}

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = com.Styles.Dialog.Spinner
	u.spinner = s

	u.keyMap.Close = CloseKey
	return u
}

// fetchUsageCmd queries the subscription plan usage endpoint.
func fetchUsageCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		usage, err := claudecode.DefaultSource().Usage(ctx)
		return usageLoadedMsg{usage: usage, err: err}
	}
}

// ID implements [Dialog].
func (u *Usage) ID() string {
	return UsageID
}

// StartLoading implements [LoadingDialog]. It kicks off both the spinner
// animation and the usage fetch.
func (u *Usage) StartLoading() tea.Cmd {
	u.loading = true
	return tea.Batch(u.spinner.Tick, fetchUsageCmd())
}

// StopLoading implements [LoadingDialog].
func (u *Usage) StopLoading() {
	u.loading = false
}

// HandleMsg implements [Dialog].
func (u *Usage) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if key.Matches(msg, u.keyMap.Close) {
			return ActionClose{}
		}
	case usageLoadedMsg:
		u.loading = false
		u.usage = msg.usage
		u.err = msg.err
	case spinner.TickMsg:
		if u.loading {
			var cmd tea.Cmd
			u.spinner, cmd = u.spinner.Update(msg)
			return ActionCmd{cmd}
		}
	}
	return nil
}

// Draw implements [Dialog].
func (u *Usage) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := u.com.Styles
	width := max(0, min(usageDialogMaxWidth, area.Dx()))

	rc := NewRenderContext(t, width)
	rc.Title = "Plan Usage"

	switch {
	case u.loading:
		rc.AddPart(u.spinner.View() + " Fetching plan usage…")
	case u.err != nil:
		rc.AddPart(t.Dialog.Quit.Content.Render("Unable to fetch plan usage:"))
		rc.AddPart(t.Dialog.Quit.Content.Render(u.err.Error()))
	default:
		for _, line := range u.usageLines() {
			rc.AddPart(line)
		}
	}

	rc.Help = "esc close"
	view := rc.Render()
	DrawCenter(scr, area, view)
	return nil
}

// usageLines renders one line per applicable plan window plus extra usage.
func (u *Usage) usageLines() []string {
	if u.usage == nil {
		return []string{"No plan usage data available."}
	}

	type window struct {
		label string
		rl    *claudecode.RateLimit
	}
	windows := []window{
		{"5-hour", u.usage.FiveHour},
		{"7-day", u.usage.SevenDay},
		{"7-day (Opus)", u.usage.SevenDayOpus},
		{"7-day (Sonnet)", u.usage.SevenDaySonnet},
		{"7-day (OAuth apps)", u.usage.SevenDayOAuthApps},
	}

	var lines []string
	for _, w := range windows {
		if w.rl == nil || w.rl.Utilization == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%-20s %s", w.label+":", formatRateLimit(w.rl)))
	}

	if eu := u.usage.ExtraUsage; eu != nil && eu.IsEnabled {
		lines = append(lines, fmt.Sprintf("%-20s %s", "Extra usage:", formatExtraUsage(eu)))
	}

	if len(lines) == 0 {
		return []string{"No plan usage data available."}
	}
	return lines
}

// formatRateLimit renders a single window as "42% used (resets in 2h13m)".
func formatRateLimit(rl *claudecode.RateLimit) string {
	pct := *rl.Utilization
	s := fmt.Sprintf("%.0f%% used", pct)
	if reset := formatReset(rl.ResetsAt); reset != "" {
		s += " (resets " + reset + ")"
	}
	return s
}

// formatExtraUsage renders extra usage credit information.
func formatExtraUsage(eu *claudecode.ExtraUsage) string {
	if eu.UsedCredits != nil && eu.MonthlyLimit != nil {
		return fmt.Sprintf("$%.2f of $%.2f used", *eu.UsedCredits, *eu.MonthlyLimit)
	}
	if eu.Utilization != nil {
		return fmt.Sprintf("%.0f%% used", *eu.Utilization)
	}
	return "enabled"
}

// formatReset turns an ISO 8601 reset timestamp into a relative "in 2h13m"
// string, falling back to the raw value when it cannot be parsed.
func formatReset(resetsAt *string) string {
	if resetsAt == nil || *resetsAt == "" {
		return ""
	}
	ts, err := time.Parse(time.RFC3339, *resetsAt)
	if err != nil {
		return *resetsAt
	}
	d := time.Until(ts)
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h >= 24:
		return fmt.Sprintf("in %dd%dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("in %dh%dm", h, m)
	default:
		return fmt.Sprintf("in %dm", m)
	}
}

// ShortHelp implements [help.KeyMap].
func (u *Usage) ShortHelp() []key.Binding {
	return []key.Binding{u.keyMap.Close}
}

// FullHelp implements [help.KeyMap].
func (u *Usage) FullHelp() [][]key.Binding {
	return [][]key.Binding{{u.keyMap.Close}}
}
