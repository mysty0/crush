package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// summaryBudget mirrors the budget selection in sessionAgent.Summarize.
// Keep in sync with the block above the fantasy.NewAgent call there.
func summaryBudget(m Model) *int64 {
	maxTokens := m.CatwalkCfg.DefaultMaxTokens
	if m.ModelCfg.MaxTokens > 0 {
		maxTokens = m.ModelCfg.MaxTokens
	}
	if maxTokens > 0 {
		return &maxTokens
	}
	return nil
}

// TestSummaryUsesModelOutputBudget guards against summaries being
// truncated mid-sentence. Summarize used to send no output limit at all,
// so providers applied their own fallback — the Anthropic provider caps
// at 4096 — cutting long summaries off and carrying the damage into the
// next context, since the summary seeds it.
func TestSummaryUsesModelOutputBudget(t *testing.T) {
	t.Parallel()

	t.Run("uses the model default", func(t *testing.T) {
		t.Parallel()
		got := summaryBudget(Model{
			CatwalkCfg: catwalk.Model{DefaultMaxTokens: 128000},
		})
		require.NotNil(t, got, "a summary must never fall back to the provider default")
		require.Equal(t, int64(128000), *got)
		require.Greater(t, *got, int64(4096), "must exceed the Anthropic 4096 fallback")
	})

	t.Run("user override wins", func(t *testing.T) {
		t.Parallel()
		got := summaryBudget(Model{
			CatwalkCfg: catwalk.Model{DefaultMaxTokens: 128000},
			ModelCfg:   config.SelectedModel{MaxTokens: 32000},
		})
		require.NotNil(t, got)
		require.Equal(t, int64(32000), *got)
	})

	t.Run("unknown budget is left unset", func(t *testing.T) {
		t.Parallel()
		// Sending a literal 0 is rejected by some providers (e.g. LM
		// Studio), so an unknown limit must omit the field entirely
		// rather than send zero.
		require.Nil(t, summaryBudget(Model{}))
	})
}
