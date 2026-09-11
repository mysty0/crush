package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/spf13/cobra"
)

var (
	ccusageHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	ccusageRuleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	ccusageTotalStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	ccusageDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

var ccusageCmd = &cobra.Command{
	Use:   "ccusage",
	Short: "Show token usage and estimated cost per day",
	Long: `Show token usage per day and model, with an estimated cost.

Numbers come from the per-turn usage recorded on each assistant message,
so sub-agent activity is included. Cost is derived from token counts and
the model's published pricing: subscription plans are not billed per
token, so Crush records their real cost as zero, and the figure shown
here is what the same usage would have cost on metered API pricing.`,
	Example: `
# Usage for every recorded day
crush ccusage

# Only the last 7 days
crush ccusage --days 7

# Machine-readable output
crush ccusage --json`,
	Args: cobra.NoArgs,
	RunE: runCCUsage,
}

func init() {
	ccusageCmd.Flags().BoolP("json", "j", false, "Output the report as JSON")
	ccusageCmd.Flags().Int("days", 0, "Only show the most recent N days (0 = all)")
	ccusageCmd.Flags().Bool("by-model", true, "Break each day down by model")
}

// ccusageEntry is one reported row: a day, optionally split by model.
type ccusageEntry struct {
	Day                 string  `json:"day"`
	Model               string  `json:"model,omitempty"`
	Provider            string  `json:"provider,omitempty"`
	Turns               int64   `json:"turns"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	Cost                float64 `json:"cost"`
	// CostEstimated marks a row whose cost had to be derived from
	// pricing because the provider bills a flat subscription rate.
	CostEstimated bool `json:"cost_estimated"`
}

// ccusageReport is the top-level --json shape.
type ccusageReport struct {
	Entries []ccusageEntry `json:"entries"`
	Total   ccusageEntry   `json:"total"`
}

func runCCUsage(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	dataDir, _ := cmd.Flags().GetString("data-dir")
	cfg, err := config.Init("", dataDir, false)
	if err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}
	if dataDir == "" {
		dataDir = cfg.Config().Options.DataDirectory
	}

	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer conn.Close()

	rows, err := db.New(conn).GetDailyTokenUsage(ctx)
	if err != nil {
		return fmt.Errorf("failed to read token usage: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("no usage recorded yet: per-turn usage is only stored on newer sessions")
	}

	byModel, _ := cmd.Flags().GetBool("by-model")
	days, _ := cmd.Flags().GetInt("days")
	entries, total := buildCCUsageReport(rows, pricingTable(cfg.Config()), byModel, days)

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		out, err := json.MarshalIndent(ccusageReport{Entries: entries, Total: total}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	printCCUsageTable(entries, total, byModel)
	return nil
}

// pricingTable indexes every configured model by ID so a recorded
// message can be priced. Subscription providers reuse the underlying
// vendor's model IDs (a "claude-code" turn runs claude-opus-5), so a
// model found under any provider is good enough to price with; the
// first one carrying real pricing wins.
func pricingTable(cfg *config.Config) map[string]catwalk.Model {
	table := map[string]catwalk.Model{}
	providers, err := config.Providers(cfg)
	if err != nil {
		return table
	}
	for _, p := range providers {
		for _, m := range p.Models {
			existing, ok := table[m.ID]
			if !ok || (!hasPricing(existing) && hasPricing(m)) {
				table[m.ID] = m
			}
		}
	}
	return table
}

func hasPricing(m catwalk.Model) bool {
	return m.CostPer1MIn > 0 || m.CostPer1MOut > 0 ||
		m.CostPer1MInCached > 0 || m.CostPer1MOutCached > 0
}

// modelCost prices a row's tokens. It mirrors the formula the agent uses
// when it bills a turn (see updateSessionUsage), so an estimated cost is
// directly comparable to a recorded one: cache writes are charged at the
// cached-input rate and cache reads at the cached-output rate.
func modelCost(m catwalk.Model, r db.DailyTokenUsageRow) float64 {
	return m.CostPer1MIn/1e6*float64(r.InputTokens) +
		m.CostPer1MOut/1e6*float64(r.OutputTokens) +
		m.CostPer1MInCached/1e6*float64(r.CacheCreationTokens) +
		m.CostPer1MOutCached/1e6*float64(r.CacheReadTokens)
}

// buildCCUsageReport turns raw per-day-per-model rows into report entries
// and a grand total, optionally collapsing models together and trimming
// to the most recent N days.
func buildCCUsageReport(rows []db.DailyTokenUsageRow, pricing map[string]catwalk.Model, byModel bool, days int) ([]ccusageEntry, ccusageEntry) {
	if days > 0 {
		rows = trimToRecentDays(rows, days)
	}

	entries := make([]ccusageEntry, 0, len(rows))
	// Collapsing by day needs an index so same-day rows merge; keyed by
	// day alone when models are not shown.
	index := map[string]int{}

	for _, r := range rows {
		cost := r.RecordedCost
		estimated := false
		if cost == 0 {
			if m, ok := pricing[r.Model]; ok {
				cost = modelCost(m, r)
				estimated = cost > 0
			}
		}

		key := r.Day
		if byModel {
			key = r.Day + "\x00" + r.Model
		}
		if i, ok := index[key]; ok {
			mergeCCUsage(&entries[i], r, cost, estimated)
			continue
		}
		e := ccusageEntry{Day: r.Day}
		if byModel {
			e.Model, e.Provider = r.Model, r.Provider
		}
		mergeCCUsage(&e, r, cost, estimated)
		index[key] = len(entries)
		entries = append(entries, e)
	}

	var total ccusageEntry
	for _, e := range entries {
		total.Turns += e.Turns
		total.InputTokens += e.InputTokens
		total.OutputTokens += e.OutputTokens
		total.CacheCreationTokens += e.CacheCreationTokens
		total.CacheReadTokens += e.CacheReadTokens
		total.TotalTokens += e.TotalTokens
		total.Cost += e.Cost
		total.CostEstimated = total.CostEstimated || e.CostEstimated
	}
	return entries, total
}

func mergeCCUsage(e *ccusageEntry, r db.DailyTokenUsageRow, cost float64, estimated bool) {
	e.Turns += r.Turns
	e.InputTokens += r.InputTokens
	e.OutputTokens += r.OutputTokens
	e.CacheCreationTokens += r.CacheCreationTokens
	e.CacheReadTokens += r.CacheReadTokens
	e.TotalTokens += r.InputTokens + r.OutputTokens + r.CacheCreationTokens + r.CacheReadTokens
	e.Cost += cost
	e.CostEstimated = e.CostEstimated || estimated
}

// trimToRecentDays keeps only rows belonging to the N most recent days.
// Rows arrive newest-day-first but with several rows per day, so days are
// counted distinctly rather than by slicing a row count.
func trimToRecentDays(rows []db.DailyTokenUsageRow, days int) []db.DailyTokenUsageRow {
	seen := map[string]bool{}
	keep := make([]db.DailyTokenUsageRow, 0, len(rows))
	for _, r := range rows {
		if !seen[r.Day] {
			if len(seen) == days {
				break
			}
			seen[r.Day] = true
		}
		keep = append(keep, r)
	}
	return keep
}

func printCCUsageTable(entries []ccusageEntry, total ccusageEntry, byModel bool) {
	headers := []string{"Date", "Model", "Turns", "Input", "Output", "Cache W", "Cache R", "Total", "Cost (USD)"}
	if !byModel {
		headers = append(headers[:1], headers[2:]...)
	}

	rows := make([][]string, 0, len(entries)+1)
	for _, e := range entries {
		rows = append(rows, ccusageRow(e, byModel))
	}
	totalRow := ccusageRow(total, byModel)
	totalRow[0] = "TOTAL"
	if byModel {
		totalRow[1] = ""
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, r := range append(rows, totalRow) {
		for i, c := range r {
			widths[i] = max(widths[i], len(c))
		}
	}

	// Everything but the first two columns is numeric, so right-align
	// those and left-align the labels.
	render := func(cells []string, style lipgloss.Style) string {
		var b strings.Builder
		for i, c := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			if i < len(cells)-7 {
				b.WriteString(c + strings.Repeat(" ", widths[i]-len(c)))
			} else {
				b.WriteString(strings.Repeat(" ", widths[i]-len(c)) + c)
			}
		}
		return style.Render(strings.TrimRight(b.String(), " "))
	}

	fmt.Println()
	fmt.Println(render(headers, ccusageHeaderStyle))
	fmt.Println(ccusageRuleStyle.Render(strings.Repeat("─", lineWidth(widths))))
	for _, r := range rows {
		fmt.Println(render(r, lipgloss.NewStyle()))
	}
	fmt.Println(ccusageRuleStyle.Render(strings.Repeat("─", lineWidth(widths))))
	fmt.Println(render(totalRow, ccusageTotalStyle))

	if total.CostEstimated {
		fmt.Println()
		fmt.Println(ccusageDimStyle.Render(
			"Cost is estimated from token counts and list pricing; subscription plans are not billed per token.",
		))
	}
}

func lineWidth(widths []int) int {
	n := 2 * (len(widths) - 1)
	for _, w := range widths {
		n += w
	}
	return n
}

func ccusageRow(e ccusageEntry, byModel bool) []string {
	cells := []string{e.Day}
	if byModel {
		cells = append(cells, e.Model)
	}
	return append(
		cells,
		humanInt(e.Turns),
		humanInt(e.InputTokens),
		humanInt(e.OutputTokens),
		humanInt(e.CacheCreationTokens),
		humanInt(e.CacheReadTokens),
		humanInt(e.TotalTokens),
		"$"+strconv.FormatFloat(e.Cost, 'f', 2, 64),
	)
}

// humanInt renders n with thousands separators.
func humanInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, d := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
