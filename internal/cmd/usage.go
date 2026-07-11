package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/client"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/charmbracelet/crush/internal/oauth/codex"
	"github.com/charmbracelet/crush/internal/oauth/geminicli"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
)

var (
	usageHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	usageLabelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	usageValueStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
)

var usageCmd = &cobra.Command{
	Use:   "usage [platform]",
	Short: "Show subscription plan usage and limits",
	Long: `Show subscription plan usage and rate limits for the OAuth providers
you are logged in to.

With no argument, usage for every logged-in subscription provider is shown.
Available platforms are: codex, gemini, antigravity.`,
	Example: `
# Show usage for all logged-in subscription providers
crush usage

# Show only OpenAI Codex usage
crush usage codex

# Machine-readable output
crush usage --json`,
	ValidArgs: []cobra.Completion{"codex", "gemini", "antigravity"},
	Args:      cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		progressEnabled := ws.Config.Options.Progress == nil || *ws.Config.Options.Progress
		if progressEnabled && supportsProgressBar() {
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
			defer func() { _, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar) }()
		}

		filter := ""
		if len(args) > 0 {
			filter = args[0]
		}
		asJSON, _ := cmd.Flags().GetBool("json")

		return runUsage(c, ws.ID, filter, asJSON)
	},
}

func init() {
	usageCmd.Flags().BoolP("json", "j", false, "Output usage reports as JSON")
}

// usageReport is the machine-readable shape emitted with --json.
type usageReport struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	PlanType string `json:"plan_type,omitempty"`
	Windows  []struct {
		Label       string  `json:"label"`
		UsedPercent float64 `json:"used_percent"`
		ResetsAt    string  `json:"resets_at,omitempty"`
	} `json:"windows,omitempty"`
	Error string `json:"error,omitempty"`
}

func runUsage(c *client.Client, wsID, filter string, asJSON bool) error {
	ctx := getLoginContext()

	cfg, err := c.GetConfig(ctx, wsID)
	if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}

	want := func(id, alias string) bool {
		return filter == "" || filter == id || filter == alias
	}

	var reports []usageReport
	if want(codex.ProviderID, "codex") {
		if pc, ok := cfg.Providers.Get(codex.ProviderID); ok && pc.OAuthToken != nil {
			reports = append(reports, codexUsageReport(ctx, c, wsID, cfg, pc))
		}
	}
	if want(geminicli.ProviderID, "gemini") {
		if pc, ok := cfg.Providers.Get(geminicli.ProviderID); ok && pc.OAuthToken != nil {
			reports = append(reports, geminiUsageReport(ctx, c, wsID, cfg, pc))
		}
	}
	if want(antigravity.ProviderID, "antigravity") {
		if pc, ok := cfg.Providers.Get(antigravity.ProviderID); ok && pc.OAuthToken != nil {
			reports = append(reports, antigravityUsageReport(ctx, c, wsID, cfg, pc))
		}
	}

	if len(reports) == 0 {
		if filter != "" {
			return fmt.Errorf("not logged in to %q, or it has no usage endpoint", filter)
		}
		fmt.Println(usageLabelStyle.Render("You are not logged in to any subscription provider with usage reporting."))
		return nil
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	}

	printUsageReports(reports)
	return nil
}

// refreshedProvider returns the provider config with a non-expired access
// token, refreshing it server-side first when the stored token is expired.
func refreshedProvider(ctx context.Context, c *client.Client, wsID string, cfg *config.Config, pc config.ProviderConfig, providerID string) config.ProviderConfig {
	if pc.OAuthToken == nil || !pc.OAuthToken.IsExpired() {
		return pc
	}
	if err := c.RefreshOAuthToken(ctx, wsID, config.ScopeGlobal, providerID); err != nil {
		return pc
	}
	newCfg, err := c.GetConfig(ctx, wsID)
	if err != nil {
		return pc
	}
	if npc, ok := newCfg.Providers.Get(providerID); ok {
		return npc
	}
	return pc
}

func codexUsageReport(ctx context.Context, c *client.Client, wsID string, cfg *config.Config, pc config.ProviderConfig) usageReport {
	pc = refreshedProvider(ctx, c, wsID, cfg, pc, codex.ProviderID)
	rep := usageReport{Provider: codex.ProviderID, Name: "OpenAI Codex"}

	u, err := codex.FetchUsage(ctx, pc.OAuthToken.AccessToken)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	rep.PlanType = u.PlanType
	addWindow := func(label string, w *codex.UsageWindow) {
		if w == nil {
			return
		}
		entry := struct {
			Label       string  `json:"label"`
			UsedPercent float64 `json:"used_percent"`
			ResetsAt    string  `json:"resets_at,omitempty"`
		}{Label: label, UsedPercent: w.UsedPercent}
		if !w.ResetsAt.IsZero() {
			entry.ResetsAt = w.ResetsAt.Format(time.RFC3339)
		}
		rep.Windows = append(rep.Windows, entry)
	}
	addWindow(codexWindowLabel(u.Primary, "Primary"), u.Primary)
	addWindow(codexWindowLabel(u.Secondary, "Secondary"), u.Secondary)
	return rep
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

func geminiUsageReport(ctx context.Context, c *client.Client, wsID string, cfg *config.Config, pc config.ProviderConfig) usageReport {
	pc = refreshedProvider(ctx, c, wsID, cfg, pc, geminicli.ProviderID)
	rep := usageReport{Provider: geminicli.ProviderID, Name: "Gemini CLI"}

	projectID := ""
	if pc.OAuthExtra != nil {
		projectID = pc.OAuthExtra["project_id"]
	}
	u, err := geminicli.FetchUsage(ctx, pc.OAuthToken.AccessToken, projectID, geminicli.GeminiCLIIdentity)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	rep.PlanType = u.Tier
	addGeminiCliWindows(&rep, u)
	return rep
}

func antigravityUsageReport(ctx context.Context, c *client.Client, wsID string, cfg *config.Config, pc config.ProviderConfig) usageReport {
	pc = refreshedProvider(ctx, c, wsID, cfg, pc, antigravity.ProviderID)
	rep := usageReport{Provider: antigravity.ProviderID, Name: "Google Antigravity"}

	projectID := ""
	if pc.OAuthExtra != nil {
		projectID = pc.OAuthExtra["project_id"]
	}
	// Antigravity shares Gemini CLI's Cloud Code Assist quota backend.
	u, err := geminicli.FetchUsage(ctx, pc.OAuthToken.AccessToken, projectID, antigravity.Identity)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	rep.PlanType = u.Tier
	addGeminiCliWindows(&rep, u)
	return rep
}

// addGeminiCliWindows appends one report window per non-disabled quota
// bucket returned by retrieveUserQuotaSummary.
func addGeminiCliWindows(rep *usageReport, u *geminicli.Usage) {
	for _, b := range u.Buckets {
		if b.Disabled {
			continue
		}
		label := b.Label
		if b.Window != "" {
			label += " (" + b.Window + ")"
		}
		usedPercent := -1.0
		if b.RemainingFraction >= 0 {
			usedPercent = (1 - b.RemainingFraction) * 100
		}
		entry := struct {
			Label       string  `json:"label"`
			UsedPercent float64 `json:"used_percent"`
			ResetsAt    string  `json:"resets_at,omitempty"`
		}{Label: label, UsedPercent: usedPercent}
		if !b.ResetsAt.IsZero() {
			entry.ResetsAt = b.ResetsAt.Format(time.RFC3339)
		}
		rep.Windows = append(rep.Windows, entry)
	}
}

func printUsageReports(reports []usageReport) {
	for i, r := range reports {
		if i > 0 {
			fmt.Println()
		}
		fmt.Println(usageHeaderStyle.Render(r.Name))
		if r.Error != "" {
			fmt.Printf("  %s %s\n", usageLabelStyle.Render("error:"), usageValueStyle.Render(r.Error))
			continue
		}
		if r.PlanType != "" {
			fmt.Printf("  %s %s\n", usageLabelStyle.Render("plan:"), usageValueStyle.Render(r.PlanType))
		}
		for _, w := range r.Windows {
			line := fmt.Sprintf("%.0f%% used", w.UsedPercent)
			if w.UsedPercent < 0 {
				line = "usage unknown"
			}
			if w.ResetsAt != "" {
				if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
					line += fmt.Sprintf(", resets in %s", humanizeUntil(t))
				}
			}
			fmt.Printf("  %s %s\n", usageLabelStyle.Render(w.Label+":"), usageValueStyle.Render(line))
		}
	}
}

// humanizeUntil renders the duration from now until t as a compact string.
func humanizeUntil(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
