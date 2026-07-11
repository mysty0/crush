package dialog

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/charmbracelet/crush/internal/oauth/codex"
	"github.com/charmbracelet/crush/internal/oauth/geminicli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUsageProviderAvailable covers every subscription provider with a
// known usage endpoint. Claude Code only needs to be present and not
// disabled (it authenticates via its own credentials file); the other
// three additionally need a stored OAuth token.
func TestUsageProviderAvailable(t *testing.T) {
	t.Parallel()

	t.Run("claude code needs no oauth token", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
		cfg.Providers.Set(claudecode.ProviderID, config.ProviderConfig{})
		assert.True(t, usageProviderAvailable(cfg, claudecode.ProviderID))
	})

	t.Run("claude code disabled", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
		cfg.Providers.Set(claudecode.ProviderID, config.ProviderConfig{Disable: true})
		assert.False(t, usageProviderAvailable(cfg, claudecode.ProviderID))
	})

	for _, id := range []string{codex.ProviderID, geminicli.ProviderID, antigravity.ProviderID} {
		t.Run(id+" needs an oauth token", func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
			cfg.Providers.Set(id, config.ProviderConfig{})
			assert.False(t, usageProviderAvailable(cfg, id), "no token yet")

			cfg.Providers.Set(id, config.ProviderConfig{OAuthToken: &oauth.Token{AccessToken: "tok"}})
			assert.True(t, usageProviderAvailable(cfg, id))
		})
	}

	t.Run("provider not configured", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
		assert.False(t, usageProviderAvailable(cfg, codex.ProviderID))
	})
}

// TestUsageAnyProviderAvailable covers the aggregate check backing the
// "Plan Usage" command's visibility: it should trigger on any configured
// subscription provider, not just the currently active one.
func TestUsageAnyProviderAvailable(t *testing.T) {
	t.Parallel()

	assert.False(t, usageAnyProviderAvailable(nil))

	empty := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	assert.False(t, usageAnyProviderAvailable(empty))

	withClaude := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	withClaude.Providers.Set(claudecode.ProviderID, config.ProviderConfig{})
	assert.True(t, usageAnyProviderAvailable(withClaude))

	withCodex := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	withCodex.Providers.Set(codex.ProviderID, config.ProviderConfig{OAuthToken: &oauth.Token{AccessToken: "tok"}})
	assert.True(t, usageAnyProviderAvailable(withCodex))
}

// TestClaudeCodeUsageLines covers the per-window rendering and the extra
// usage credit line.
func TestClaudeCodeUsageLines(t *testing.T) {
	t.Parallel()

	assert.Nil(t, claudeCodeUsageLines(nil))

	pct := 42.0
	usage := &claudecode.Utilization{
		FiveHour: &claudecode.RateLimit{Utilization: &pct},
		ExtraUsage: &claudecode.ExtraUsage{
			IsEnabled:   true,
			Utilization: &pct,
		},
	}
	lines := claudeCodeUsageLines(usage)
	require.Len(t, lines, 2)
	assert.Equal(t, "5-hour:", lines[0].label)
	assert.Equal(t, "42% used", lines[0].value)
	assert.Equal(t, "Extra usage:", lines[1].label)
	assert.Equal(t, "42% used", lines[1].value)
}

// TestClaudeCodeUsageLines_SkipsDisabledExtraUsage covers a plan with no
// applicable windows and disabled extra usage producing no lines.
func TestClaudeCodeUsageLines_SkipsDisabledExtraUsage(t *testing.T) {
	t.Parallel()

	usage := &claudecode.Utilization{
		ExtraUsage: &claudecode.ExtraUsage{IsEnabled: false},
	}
	assert.Empty(t, claudeCodeUsageLines(usage))
}

// TestCodexUsageLines covers the plan type line and both rate-limit
// windows, including the duration-based window label.
func TestCodexUsageLines(t *testing.T) {
	t.Parallel()

	assert.Nil(t, codexUsageLines(nil))

	u := &codex.Usage{
		PlanType: "pro",
		Primary: &codex.UsageWindow{
			UsedPercent:   50,
			WindowSeconds: int64((5 * time.Hour).Seconds()),
			ResetsAt:      time.Now().Add(2 * time.Hour),
		},
		Secondary: &codex.UsageWindow{
			UsedPercent:   -1,
			WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
		},
	}
	lines := codexUsageLines(u)
	require.Len(t, lines, 3)
	assert.Equal(t, "Plan:", lines[0].label)
	assert.Equal(t, "pro", lines[0].value)
	assert.Equal(t, "5h window:", lines[1].label)
	assert.Contains(t, lines[1].value, "50% used")
	assert.Contains(t, lines[1].value, "resets in")
	assert.Equal(t, "7d window:", lines[2].label)
	assert.Equal(t, "usage unknown", lines[2].value)
}

// TestGeminiCliUsageLines covers the tier line and per-bucket rendering,
// including a bucket with an unknown remaining fraction and a disabled
// bucket being skipped.
func TestGeminiCliUsageLines(t *testing.T) {
	t.Parallel()

	assert.Nil(t, geminiCliUsageLines(nil))

	u := &geminicli.Usage{
		Tier: "free-tier",
		Buckets: []geminicli.UsageBucket{
			{Label: "Gemini 2.5 Pro", Window: "24h", RemainingFraction: 0.75, ResetsAt: time.Now().Add(time.Hour)},
			{Label: "Gemini 2.5 Flash", RemainingFraction: -1},
			{Label: "Legacy", Disabled: true},
		},
	}
	lines := geminiCliUsageLines(u)
	require.Len(t, lines, 3)

	assert.Equal(t, "Tier:", lines[0].label)
	assert.Equal(t, "free-tier", lines[0].value)

	assert.Equal(t, "Gemini 2.5 Pro (24h):", lines[1].label)
	assert.Contains(t, lines[1].value, "75% remaining")
	assert.Contains(t, lines[1].value, "resets in")

	assert.Equal(t, "Gemini 2.5 Flash:", lines[2].label)
	assert.Equal(t, "usage unknown", lines[2].value)
}

// TestHumanizeUntilDuration covers the compact duration formatting used
// for Claude Code, Codex, and Gemini CLI reset times.
func TestHumanizeUntilDuration(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "now", humanizeUntilDuration(0))
	assert.Equal(t, "now", humanizeUntilDuration(-time.Minute))
	assert.Equal(t, "5m", humanizeUntilDuration(5*time.Minute))
	assert.Equal(t, "2h13m", humanizeUntilDuration(2*time.Hour+13*time.Minute))
	assert.Equal(t, "1d2h", humanizeUntilDuration(26*time.Hour))
}

// TestMergeSharedAccountSections covers the Gemini CLI / Antigravity
// merge: since both providers share the exact same Cloud Code Assist
// backend and account quota, showing byte-identical usage under two
// separate headings looks like duplicated "5h"/"weekly" limits rather
// than the same limits reported twice. Sections should be merged when
// they match and left alone otherwise (different data, an error on
// either side, or either provider missing).
func TestMergeSharedAccountSections(t *testing.T) {
	t.Parallel()

	sameLines := []usageLine{{"Gemini 2.5 Pro — 5h:", "90% remaining"}}

	t.Run("merges when both report identical data", func(t *testing.T) {
		t.Parallel()
		sections := []usageSection{
			{providerName: "Claude Code", lines: []usageLine{{"5-hour:", "10% used"}}},
			{providerName: "Gemini CLI", lines: sameLines},
			{providerName: "Google Antigravity", lines: sameLines},
		}
		merged := mergeSharedAccountSections(sections)
		require.Len(t, merged, 2)
		assert.Equal(t, "Claude Code", merged[0].providerName)
		assert.Equal(t, "Gemini CLI / Antigravity", merged[1].providerName)
		assert.Equal(t, sameLines, merged[1].lines)
	})

	t.Run("left separate when data differs", func(t *testing.T) {
		t.Parallel()
		sections := []usageSection{
			{providerName: "Gemini CLI", lines: []usageLine{{"Gemini 2.5 Pro — 5h:", "90% remaining"}}},
			{providerName: "Google Antigravity", lines: []usageLine{{"Gemini 2.5 Pro — 5h:", "50% remaining"}}},
		}
		merged := mergeSharedAccountSections(sections)
		require.Len(t, merged, 2)
		assert.Equal(t, "Gemini CLI", merged[0].providerName)
		assert.Equal(t, "Google Antigravity", merged[1].providerName)
	})

	t.Run("left separate when either side errored", func(t *testing.T) {
		t.Parallel()
		sections := []usageSection{
			{providerName: "Gemini CLI", lines: sameLines},
			{providerName: "Google Antigravity", err: assert.AnError},
		}
		merged := mergeSharedAccountSections(sections)
		require.Len(t, merged, 2)
	})

	t.Run("no-op when only one is present", func(t *testing.T) {
		t.Parallel()
		sections := []usageSection{{providerName: "Gemini CLI", lines: sameLines}}
		merged := mergeSharedAccountSections(sections)
		require.Len(t, merged, 1)
		assert.Equal(t, "Gemini CLI", merged[0].providerName)
	})
}
