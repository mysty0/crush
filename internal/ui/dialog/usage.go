package dialog

import (
	"context"
	"fmt"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/charmbracelet/crush/internal/oauth/codex"
	"github.com/charmbracelet/crush/internal/oauth/geminicli"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

// UsageID is the identifier for the plan usage dialog.
const (
	UsageID             = "usage"
	usageDialogMaxWidth = 60
)

// usageProviderName returns the display name of the subscription
// provider whose usage the dialog should show, or "" when the current
// large model's provider has no known usage endpoint. It backs both the
// "Plan Usage" command's visibility and the dialog's title.
func usageProviderName(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	switch cfg.Models[config.SelectedModelTypeLarge].Provider {
	case claudecode.ProviderID:
		return "Claude Code"
	case codex.ProviderID:
		return "OpenAI Codex"
	case geminicli.ProviderID:
		return "Gemini CLI"
	case antigravity.ProviderID:
		return "Google Antigravity"
	default:
		return ""
	}
}

// usageLine is one rendered row of the plan usage view: a label and a
// formatted value, e.g. {"5-hour:", "42% used (resets in 2h13m)"}.
type usageLine struct {
	label string
	value string
}

// usageLoadedMsg carries the result of the asynchronous plan usage fetch.
type usageLoadedMsg struct {
	lines []usageLine
	err   error
}

// Usage is a read-only dialog that shows the current subscription
// provider's plan usage limits: Claude Code, OpenAI Codex, Gemini CLI, or
// Google Antigravity. It is only meaningful when the active large model's
// provider is one of these OAuth subscription providers (see
// usageProviderName).
type Usage struct {
	com     *common.Common
	spinner spinner.Model
	loading bool

	providerName string
	lines        []usageLine
	err          error

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
	u := &Usage{com: com, loading: true, providerName: usageProviderName(com.Config())}

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = com.Styles.Dialog.Spinner
	u.spinner = s

	u.keyMap.Close = CloseKey
	return u
}

// fetchUsageCmd queries the active subscription provider's plan usage
// endpoint, refreshing its stored OAuth token first if it has expired.
func (u *Usage) fetchUsageCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		cfg := u.com.Config()
		if cfg == nil {
			return usageLoadedMsg{err: fmt.Errorf("configuration not found")}
		}

		providerID := cfg.Models[config.SelectedModelTypeLarge].Provider
		switch providerID {
		case claudecode.ProviderID:
			usage, err := claudecode.DefaultSource().Usage(ctx)
			if err != nil {
				return usageLoadedMsg{err: err}
			}
			return usageLoadedMsg{lines: claudeCodeUsageLines(usage)}
		case codex.ProviderID, geminicli.ProviderID, antigravity.ProviderID:
			pc, ok := cfg.Providers.Get(providerID)
			if !ok || pc.OAuthToken == nil {
				return usageLoadedMsg{err: fmt.Errorf("not logged in to %s", providerID)}
			}
			if pc.OAuthToken.IsExpired() {
				if err := u.com.Workspace.RefreshOAuthToken(ctx, config.ScopeGlobal, providerID); err == nil {
					if refreshed, ok := u.com.Config().Providers.Get(providerID); ok {
						pc = refreshed
					}
				}
			}
			return fetchOAuthUsage(ctx, providerID, pc)
		default:
			return usageLoadedMsg{err: fmt.Errorf("plan usage is not available for this provider")}
		}
	}
}

// fetchOAuthUsage dispatches to the provider-specific FetchUsage call for
// the Codex/Gemini-CLI-family subscription providers, translating the
// result into display lines.
func fetchOAuthUsage(ctx context.Context, providerID string, pc config.ProviderConfig) usageLoadedMsg {
	switch providerID {
	case codex.ProviderID:
		u, err := codex.FetchUsage(ctx, pc.OAuthToken.AccessToken)
		if err != nil {
			return usageLoadedMsg{err: err}
		}
		return usageLoadedMsg{lines: codexUsageLines(u)}
	case geminicli.ProviderID, antigravity.ProviderID:
		projectID := ""
		if pc.OAuthExtra != nil {
			projectID = pc.OAuthExtra["project_id"]
		}
		identity := geminicli.GeminiCLIIdentity
		if providerID == antigravity.ProviderID {
			identity = antigravity.Identity
		}
		u, err := geminicli.FetchUsage(ctx, pc.OAuthToken.AccessToken, projectID, identity)
		if err != nil {
			return usageLoadedMsg{err: err}
		}
		return usageLoadedMsg{lines: geminiCliUsageLines(u)}
	default:
		return usageLoadedMsg{err: fmt.Errorf("plan usage is not available for this provider")}
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
	return tea.Batch(u.spinner.Tick, u.fetchUsageCmd())
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
		u.lines = msg.lines
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
	if u.providerName != "" {
		rc.Title = u.providerName + " Plan Usage"
	}

	switch {
	case u.loading:
		rc.AddPart(u.spinner.View() + " Fetching plan usage…")
	case u.err != nil:
		rc.AddPart(t.Dialog.Quit.Content.Render("Unable to fetch plan usage:"))
		rc.AddPart(t.Dialog.Quit.Content.Render(u.err.Error()))
	case len(u.lines) == 0:
		rc.AddPart("No plan usage data available.")
	default:
		for _, l := range u.lines {
			rc.AddPart(fmt.Sprintf("%-20s %s", l.label, l.value))
		}
	}

	rc.Help = "esc close"
	view := rc.Render()
	DrawCenter(scr, area, view)
	return nil
}

// claudeCodeUsageLines renders one line per applicable Claude Code plan
// window plus extra usage.
func claudeCodeUsageLines(usage *claudecode.Utilization) []usageLine {
	if usage == nil {
		return nil
	}

	type window struct {
		label string
		rl    *claudecode.RateLimit
	}
	windows := []window{
		{"5-hour:", usage.FiveHour},
		{"7-day:", usage.SevenDay},
		{"7-day (Opus):", usage.SevenDayOpus},
		{"7-day (Sonnet):", usage.SevenDaySonnet},
		{"7-day (OAuth apps):", usage.SevenDayOAuthApps},
	}

	var lines []usageLine
	for _, w := range windows {
		if w.rl == nil || w.rl.Utilization == nil {
			continue
		}
		lines = append(lines, usageLine{w.label, formatRateLimit(w.rl)})
	}

	if eu := usage.ExtraUsage; eu != nil && eu.IsEnabled {
		lines = append(lines, usageLine{"Extra usage:", formatExtraUsage(eu)})
	}
	return lines
}

// codexUsageLines renders the OpenAI Codex plan type and its rate-limit
// windows.
func codexUsageLines(u *codex.Usage) []usageLine {
	if u == nil {
		return nil
	}
	var lines []usageLine
	if u.PlanType != "" {
		lines = append(lines, usageLine{"Plan:", u.PlanType})
	}
	addWindow := func(label string, w *codex.UsageWindow) {
		if w == nil {
			return
		}
		value := fmt.Sprintf("%.0f%% used", w.UsedPercent)
		if w.UsedPercent < 0 {
			value = "usage unknown"
		}
		if !w.ResetsAt.IsZero() {
			value += ", resets " + formatResetTime(w.ResetsAt)
		}
		lines = append(lines, usageLine{codexWindowLabel(w, label) + ":", value})
	}
	addWindow("Primary", u.Primary)
	addWindow("Secondary", u.Secondary)
	return lines
}

// geminiCliUsageLines renders the Gemini CLI / Antigravity plan tier and
// remaining quota fraction.
func geminiCliUsageLines(u *geminicli.Usage) []usageLine {
	if u == nil {
		return nil
	}
	var lines []usageLine
	if u.Tier != "" {
		lines = append(lines, usageLine{"Tier:", u.Tier})
	}
	if u.RemainingFraction >= 0 {
		lines = append(lines, usageLine{"Remaining:", fmt.Sprintf("%.0f%%", u.RemainingFraction*100)})
	}
	return lines
}

// codexWindowLabel names a window by its duration when known.
func codexWindowLabel(w *codex.UsageWindow, fallback string) string {
	if w == nil || w.WindowSeconds <= 0 {
		return fallback
	}
	d := time.Duration(w.WindowSeconds) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd window", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dh window", int(d.Hours()))
	}
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
	return "in " + humanizeUntilDuration(time.Until(ts))
}

// formatResetTime renders an absolute reset time as a relative "in
// 2h13m" string.
func formatResetTime(t time.Time) string {
	return "in " + humanizeUntilDuration(time.Until(t))
}

// humanizeUntilDuration renders a duration as a compact "2h13m"-style
// string, or "now" when it has already elapsed.
func humanizeUntilDuration(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h >= 24:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
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
