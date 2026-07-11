package config

import "charm.land/catwalk/pkg/catwalk"

// ThinkingBudgetLevels enumerates the discrete extended-thinking
// presets offered for providers that configure thinking via a numeric
// token budget or discrete level (Anthropic direct, Bedrock, Google)
// rather than catwalk-supplied named effort levels (as OpenAI-style
// reasoning models use). Selecting a level sets
// SelectedModel.ReasoningEffort to its name; the actual budget/level is
// resolved downstream (ThinkingBudgetTokens for Anthropic/Bedrock/
// Gemini 2.x's numeric budget, or a provider-specific level mapping for
// Gemini 3+). "off" disables thinking outright, mirroring the
// low/medium/high/off intensity picker Claude Code offers instead of a
// plain on/off toggle.
var ThinkingBudgetLevels = []string{"off", "low", "medium", "high"}

// ThinkingBudgetTokens returns the token budget for one of
// ThinkingBudgetLevels. Returns 0 for "off", an unrecognized level, or
// an empty string -- all of which mean "don't set a budget".
func ThinkingBudgetTokens(level string) int64 {
	switch level {
	case "low":
		return 4096
	case "medium":
		return 12000
	case "high":
		return 32000
	default:
		return 0
	}
}

// UsesThinkingBudget reports whether a provider configures extended
// thinking via the discrete off/low/medium/high picker (ThinkingBudgetLevels)
// rather than catwalk-supplied named reasoning-effort levels. This
// includes Google (Gemini's thinking is a numeric token budget on 2.x
// models and a discrete level on 3+ models -- both map onto this same
// picker; see the google.Name case in coordinator.go's
// getProviderOptions).
func UsesThinkingBudget(providerType catwalk.Type) bool {
	return providerType == catwalk.TypeAnthropic ||
		providerType == catwalk.TypeBedrock ||
		providerType == catwalk.TypeGoogle
}
