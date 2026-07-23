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

// usageProvider is one subscription account to report plan usage for.
type usageProvider struct {
	id   string
	name string
}

// usageProviders lists the single-account subscription providers with a
// known plan usage endpoint, in display order. Claude Code is absent
// because a user can be signed in to several Claude subscriptions at
// once; those accounts are enumerated from config instead (see
// usageSubscriptionProviders).
var usageProviders = []usageProvider{
	{codex.ProviderID, "OpenAI Codex"},
	{geminicli.ProviderID, "Gemini CLI"},
	{antigravity.ProviderID, "Google Antigravity"},
}

// usageProviderAvailable reports whether the given single-account
// subscription provider is configured and ready to report usage.
func usageProviderAvailable(cfg *config.Config, providerID string) bool {
	pc, ok := cfg.Providers.Get(providerID)
	if !ok || pc.Disable {
		return false
	}
	return pc.OAuthToken != nil
}

// usageSubscriptionProviders returns every subscription account to report
// usage for, in display order: each configured Claude subscription first
// (one section per account, since each has its own limits), then the
// single-account providers.
func usageSubscriptionProviders(cfg *config.Config) []usageProvider {
	if cfg == nil {
		return nil
	}
	var providers []usageProvider
	for _, id := range cfg.ClaudeCodeAccounts() {
		name := "Claude Code"
		if pc, ok := cfg.Providers.Get(id); ok && pc.Name != "" {
			name = pc.Name
		}
		providers = append(providers, usageProvider{id: id, name: name})
	}
	for _, p := range usageProviders {
		if usageProviderAvailable(cfg, p.id) {
			providers = append(providers, p)
		}
	}
	return providers
}

// usageAnyProviderAvailable reports whether any subscription provider
// with a known usage endpoint is configured, regardless of which one (if
// any) is currently selected as the active model's provider. It backs
// the "Plan Usage" command's visibility.
func usageAnyProviderAvailable(cfg *config.Config) bool {
	return len(usageSubscriptionProviders(cfg)) > 0
}

// usageLine is one rendered row of the plan usage view: a label and a
// formatted value, e.g. {"5-hour:", "42% used (resets in 2h13m)"}.
type usageLine struct {
	label string
	value string
}

// usageSection is one provider's plan usage, rendered as a header
// followed by its lines. err is set when the fetch failed; lines is
// empty (with no err) when the provider reported no usage data.
type usageSection struct {
	providerName string
	lines        []usageLine
	err          error
}

// usageLoadedMsg carries the result of the asynchronous plan usage
// fetch across every configured subscription provider.
type usageLoadedMsg struct {
	sections []usageSection
}

// Usage is a read-only dialog that shows plan usage limits for every
// configured subscription provider: Claude Code, OpenAI Codex, Gemini
// CLI, and Google Antigravity.
type Usage struct {
	com     *common.Common
	spinner spinner.Model
	loading bool

	sections []usageSection

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

// fetchUsageCmd queries the plan usage endpoint of every configured
// subscription provider, refreshing each one's stored OAuth token first
// if it has expired.
func (u *Usage) fetchUsageCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		cfg := u.com.Config()
		if cfg == nil {
			return usageLoadedMsg{sections: []usageSection{{err: fmt.Errorf("configuration not found")}}}
		}

		var sections []usageSection
		for _, p := range usageSubscriptionProviders(cfg) {
			lines, err := u.fetchProviderUsage(ctx, cfg, p.id)
			sections = append(sections, usageSection{providerName: p.name, lines: lines, err: err})
		}
		return usageLoadedMsg{sections: mergeSharedAccountSections(sections)}
	}
}

// mergeSharedAccountSections collapses the Gemini CLI and Antigravity
// sections into one when both are present and report identical data.
// The two providers are confirmed to share the exact same Cloud Code
// Assist backend and account quota (see
// docs/antigravity-cli-oauth-findings.md), so when both are logged in
// with the same underlying Google account they always report the same
// tier and buckets; showing that data twice under two different
// headings looks like duplicated "5h"/"weekly" limits rather than the
// same limits reported by two names for the same subscription. If the
// two logins somehow resolved to different accounts (different results),
// they are left as separate sections since merging would be wrong.
func mergeSharedAccountSections(sections []usageSection) []usageSection {
	geminiIdx, antigravityIdx := -1, -1
	for i, s := range sections {
		switch s.providerName {
		case "Gemini CLI":
			geminiIdx = i
		case "Google Antigravity":
			antigravityIdx = i
		}
	}
	if geminiIdx == -1 || antigravityIdx == -1 {
		return sections
	}

	gemini, antigravity := sections[geminiIdx], sections[antigravityIdx]
	if gemini.err != nil || antigravity.err != nil || !usageLinesEqual(gemini.lines, antigravity.lines) {
		return sections
	}

	merged := make([]usageSection, 0, len(sections)-1)
	for i, s := range sections {
		if i == antigravityIdx {
			continue
		}
		if i == geminiIdx {
			s.providerName = "Gemini CLI / Antigravity"
		}
		merged = append(merged, s)
	}
	return merged
}

// usageLinesEqual reports whether two usage line slices carry the same
// labels and values, regardless of order sensitivity (both sides come
// from the same code path so ordering is already stable).
func usageLinesEqual(a, b []usageLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fetchProviderUsage fetches and formats plan usage for a single
// subscription provider.
func (u *Usage) fetchProviderUsage(ctx context.Context, cfg *config.Config, providerID string) ([]usageLine, error) {
	if claudecode.IsProviderID(providerID) {
		usage, err := u.claudeCodeSource(ctx, cfg, providerID).Usage(ctx)
		if err != nil {
			return nil, err
		}
		return claudeCodeUsageLines(usage), nil
	}

	pc, _ := cfg.Providers.Get(providerID)
	if pc.OAuthToken.IsExpired() {
		if err := u.com.Workspace.RefreshOAuthToken(ctx, config.ScopeGlobal, providerID); err == nil {
			if refreshed, ok := u.com.Config().Providers.Get(providerID); ok {
				pc = refreshed
			}
		}
	}
	return fetchOAuthUsageLines(ctx, providerID, pc)
}

// claudeCodeSource returns the token source for one Claude subscription
// account. Accounts added with `crush login claude-code` authenticate
// with the token stored in config, refreshed over the workspace
// connection first if it has expired; the default account falls back to
// the credentials file the official Claude Code CLI maintains.
func (u *Usage) claudeCodeSource(ctx context.Context, cfg *config.Config, providerID string) *claudecode.Source {
	pc, ok := cfg.Providers.Get(providerID)
	if !ok || pc.OAuthToken == nil {
		return claudecode.DefaultSource()
	}
	if pc.OAuthToken.IsExpired() {
		if err := u.com.Workspace.RefreshOAuthToken(ctx, config.ScopeGlobal, providerID); err == nil {
			if refreshed, ok := u.com.Config().Providers.Get(providerID); ok {
				pc = refreshed
			}
		}
	}
	token := pc.OAuthToken.AccessToken
	return claudecode.NewTokenSource(claudecode.TokenFunc(
		func(context.Context) (string, error) { return token, nil },
	))
}

// fetchOAuthUsageLines dispatches to the provider-specific FetchUsage
// call for the Codex/Gemini-CLI-family subscription providers,
// translating the result into display lines.
func fetchOAuthUsageLines(ctx context.Context, providerID string, pc config.ProviderConfig) ([]usageLine, error) {
	switch providerID {
	case codex.ProviderID:
		u, err := codex.FetchUsage(ctx, pc.OAuthToken.AccessToken)
		if err != nil {
			return nil, err
		}
		return codexUsageLines(u), nil
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
			return nil, err
		}
		return geminiCliUsageLines(u), nil
	default:
		return nil, fmt.Errorf("plan usage is not available for this provider")
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
		u.sections = msg.sections
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
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()

	rc := NewRenderContext(t, width)
	rc.Title = "Plan Usage"

	switch {
	case u.loading:
		rc.AddPart(u.spinner.View() + " Fetching plan usage…")
	case len(u.sections) == 0:
		rc.AddPart("You are not logged in to any subscription provider with usage reporting.")
	default:
		for i, s := range u.sections {
			if i > 0 {
				rc.AddPart("")
			}
			rc.AddPart(common.Section(t, s.providerName, innerWidth))
			switch {
			case s.err != nil:
				rc.AddPart(t.Dialog.Quit.Content.Render("Unable to fetch plan usage: " + s.err.Error()))
			case len(s.lines) == 0:
				rc.AddPart("No plan usage data available.")
			default:
				for _, l := range s.lines {
					rc.AddPart(fmt.Sprintf("%-20s %s", l.label, l.value))
				}
			}
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
// each quota bucket (one per model or feature group) returned by
// retrieveUserQuotaSummary.
func geminiCliUsageLines(u *geminicli.Usage) []usageLine {
	if u == nil {
		return nil
	}
	var lines []usageLine
	if u.Tier != "" {
		lines = append(lines, usageLine{"Tier:", u.Tier})
	}
	for _, b := range u.Buckets {
		if b.Disabled {
			continue
		}
		label := b.Label
		if b.Window != "" {
			label += " (" + b.Window + ")"
		}
		value := "usage unknown"
		if b.RemainingFraction >= 0 {
			value = fmt.Sprintf("%.0f%% remaining", b.RemainingFraction*100)
		}
		if !b.ResetsAt.IsZero() {
			value += ", resets " + formatResetTime(b.ResetsAt)
		}
		lines = append(lines, usageLine{label + ":", value})
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
