package dialog

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/charmbracelet/crush/internal/oauth/codex"
	"github.com/charmbracelet/crush/internal/oauth/geminicli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUsageProviderName covers every subscription provider with a known
// usage endpoint, plus the API-key/unknown-provider case where the "Plan
// Usage" command should not be offered at all.
func TestUsageProviderName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		providerID string
		want       string
	}{
		{"claude code", claudecode.ProviderID, "Claude Code"},
		{"codex", codex.ProviderID, "OpenAI Codex"},
		{"gemini cli", geminicli.ProviderID, "Gemini CLI"},
		{"antigravity", antigravity.ProviderID, "Google Antigravity"},
		{"unknown provider", "some-api-key-provider", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Models: map[config.SelectedModelType]config.SelectedModel{
					config.SelectedModelTypeLarge: {Provider: tc.providerID},
				},
			}
			assert.Equal(t, tc.want, usageProviderName(cfg))
		})
	}

	t.Run("nil config", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "", usageProviderName(nil))
	})
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

// TestGeminiCliUsageLines covers the tier and remaining-quota lines,
// including the "unknown remaining" case (-1) being omitted.
func TestGeminiCliUsageLines(t *testing.T) {
	t.Parallel()

	assert.Nil(t, geminiCliUsageLines(nil))

	u := &geminicli.Usage{Tier: "free-tier", RemainingFraction: 0.75}
	lines := geminiCliUsageLines(u)
	require.Len(t, lines, 2)
	assert.Equal(t, "Tier:", lines[0].label)
	assert.Equal(t, "free-tier", lines[0].value)
	assert.Equal(t, "Remaining:", lines[1].label)
	assert.Equal(t, "75%", lines[1].value)

	unknown := &geminicli.Usage{Tier: "standard-tier", RemainingFraction: -1}
	lines = geminiCliUsageLines(unknown)
	require.Len(t, lines, 1)
	assert.Equal(t, "Tier:", lines[0].label)
}

// TestHumanizeUntilDuration covers the compact duration formatting used
// for both Claude Code and Codex reset times.
func TestHumanizeUntilDuration(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "now", humanizeUntilDuration(0))
	assert.Equal(t, "now", humanizeUntilDuration(-time.Minute))
	assert.Equal(t, "5m", humanizeUntilDuration(5*time.Minute))
	assert.Equal(t, "2h13m", humanizeUntilDuration(2*time.Hour+13*time.Minute))
	assert.Equal(t, "1d2h", humanizeUntilDuration(26*time.Hour))
}
