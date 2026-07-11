package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/assert"
)

func TestThinkingBudgetTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level string
		want  int64
	}{
		{"low", 4096},
		{"medium", 12000},
		{"high", 32000},
		{"off", 0},
		{"", 0},
		{"unknown", 0},
	}
	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ThinkingBudgetTokens(tc.level))
		})
	}
}

func TestThinkingBudgetTokens_LevelsAreOrdered(t *testing.T) {
	t.Parallel()

	// low < medium < high, so the picker reads as an actual intensity
	// slider rather than an arbitrary list.
	low := ThinkingBudgetTokens("low")
	medium := ThinkingBudgetTokens("medium")
	high := ThinkingBudgetTokens("high")
	assert.Less(t, low, medium)
	assert.Less(t, medium, high)
}

func TestUsesThinkingBudget(t *testing.T) {
	t.Parallel()

	assert.True(t, UsesThinkingBudget(catwalk.TypeAnthropic))
	assert.True(t, UsesThinkingBudget(catwalk.TypeBedrock))
	assert.False(t, UsesThinkingBudget(catwalk.TypeOpenAI))
	assert.False(t, UsesThinkingBudget(catwalk.TypeOpenAICompat))
	assert.False(t, UsesThinkingBudget(catwalk.TypeOpenRouter))
	assert.False(t, UsesThinkingBudget(catwalk.TypeVercel))
	assert.False(t, UsesThinkingBudget(catwalk.TypeGoogle))
	assert.False(t, UsesThinkingBudget(catwalk.TypeAzure))
}

func TestThinkingBudgetLevels_IncludesOff(t *testing.T) {
	t.Parallel()

	assert.Contains(t, ThinkingBudgetLevels, "off")
	assert.Equal(t, "off", ThinkingBudgetLevels[0], "off should be the first/lowest picker entry")
}
