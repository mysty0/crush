package cmd

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

func usageRow(day, model string, in, out, cacheW, cacheR int64, cost float64) db.DailyTokenUsageRow {
	return db.DailyTokenUsageRow{
		Day:                 day,
		Model:               model,
		Provider:            "claude-code",
		Turns:               1,
		InputTokens:         in,
		OutputTokens:        out,
		CacheCreationTokens: cacheW,
		CacheReadTokens:     cacheR,
		RecordedCost:        cost,
	}
}

var opus = map[string]catwalk.Model{
	"claude-opus-5": {
		ID:                 "claude-opus-5",
		CostPer1MIn:        5,
		CostPer1MOut:       25,
		CostPer1MInCached:  6.25,
		CostPer1MOutCached: 0.5,
	},
}

// TestModelCostMatchesAgentFormula pins the pricing arithmetic to the
// same shape the agent uses when it bills a live turn, so an estimated
// cost stays comparable with a recorded one. Cache writes are charged at
// the cached-input rate and cache reads at the cached-output rate.
func TestModelCostMatchesAgentFormula(t *testing.T) {
	t.Parallel()

	got := modelCost(opus["claude-opus-5"], usageRow("d", "claude-opus-5", 194, 52090, 460348, 17672581, 0))
	require.InDelta(t, 13.0166855, got, 1e-9)
}

// TestBuildReportEstimatesCostForSubscriptions covers the central reason
// this report exists: flat-rate subscription turns are recorded with a
// cost of zero, so the report has to derive one from token counts.
func TestBuildReportEstimatesCostForSubscriptions(t *testing.T) {
	t.Parallel()

	entries, total := buildCCUsageReport(
		[]db.DailyTokenUsageRow{usageRow("2026-08-20", "claude-opus-5", 0, 1_000_000, 0, 0, 0)},
		opus, true, 0,
	)
	require.Len(t, entries, 1)
	require.True(t, entries[0].CostEstimated)
	require.InDelta(t, 25.0, entries[0].Cost, 1e-9)
	require.InDelta(t, 25.0, total.Cost, 1e-9)
}

// TestBuildReportKeepsRecordedCost checks that a metered provider's real
// billed cost is reported as-is and never overwritten by an estimate.
func TestBuildReportKeepsRecordedCost(t *testing.T) {
	t.Parallel()

	entries, _ := buildCCUsageReport(
		[]db.DailyTokenUsageRow{usageRow("2026-08-20", "claude-opus-5", 0, 1_000_000, 0, 0, 3.50)},
		opus, true, 0,
	)
	require.InDelta(t, 3.50, entries[0].Cost, 1e-9)
	require.False(t, entries[0].CostEstimated, "a real billed cost is not an estimate")
}

// TestBuildReportCollapsesModels checks the --by-model=false path merges
// a day's models into one row while preserving totals.
func TestBuildReportCollapsesModels(t *testing.T) {
	t.Parallel()

	rows := []db.DailyTokenUsageRow{
		usageRow("2026-08-20", "claude-opus-5", 10, 20, 30, 40, 1),
		usageRow("2026-08-20", "claude-sonnet-5", 1, 2, 3, 4, 2),
	}

	split, _ := buildCCUsageReport(rows, opus, true, 0)
	require.Len(t, split, 2)

	merged, total := buildCCUsageReport(rows, opus, false, 0)
	require.Len(t, merged, 1)
	require.Equal(t, int64(2), merged[0].Turns)
	require.Equal(t, int64(11), merged[0].InputTokens)
	require.Equal(t, int64(110), merged[0].TotalTokens)
	require.Empty(t, merged[0].Model, "a collapsed row is not attributable to one model")
	require.InDelta(t, 3.0, total.Cost, 1e-9)
}

// TestTrimCountsDistinctDays guards the --days flag: rows arrive several
// per day, so trimming must count days rather than slice a row count.
func TestTrimCountsDistinctDays(t *testing.T) {
	t.Parallel()

	rows := []db.DailyTokenUsageRow{
		usageRow("2026-08-20", "a", 1, 1, 1, 1, 0),
		usageRow("2026-08-20", "b", 1, 1, 1, 1, 0),
		usageRow("2026-08-20", "c", 1, 1, 1, 1, 0),
		usageRow("2026-08-19", "a", 1, 1, 1, 1, 0),
		usageRow("2026-08-18", "a", 1, 1, 1, 1, 0),
	}

	got := trimToRecentDays(rows, 2)
	require.Len(t, got, 4, "all three rows of the newest day plus the next day")
	for _, r := range got {
		require.NotEqual(t, "2026-08-18", r.Day)
	}

	require.Len(t, trimToRecentDays(rows, 99), len(rows), "asking for more days than exist keeps everything")
}

// TestUnknownModelIsNotPricedAsZeroCost makes sure an unrecognized model
// is reported without a fabricated cost rather than silently priced at 0
// and marked estimated.
func TestUnknownModelIsNotPricedAsZeroCost(t *testing.T) {
	t.Parallel()

	entries, _ := buildCCUsageReport(
		[]db.DailyTokenUsageRow{usageRow("2026-08-20", "some-unknown-model", 1, 1, 1, 1, 0)},
		opus, true, 0,
	)
	require.Zero(t, entries[0].Cost)
	require.False(t, entries[0].CostEstimated, "no pricing means no estimate claim")
}

func TestHumanInt(t *testing.T) {
	t.Parallel()

	for in, want := range map[int64]string{
		0: "0", 12: "12", 999: "999", 1000: "1,000",
		171322359: "171,322,359", -4567: "-4,567",
	} {
		require.Equal(t, want, humanInt(in))
	}
}
