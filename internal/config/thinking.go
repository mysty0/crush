package config

import "charm.land/catwalk/pkg/catwalk"

// ThinkingBudgetLevels enumerates the discrete extended-thinking
// presets offered for providers that configure thinking via a numeric
// token budget (Anthropic direct, Bedrock) rather than catwalk-supplied
// named effort levels (as OpenAI-style reasoning models use). Selecting
// a level sets SelectedModel.ReasoningEffort to its name; the actual
// budget is resolved by ThinkingBudgetTokens. "off" disables thinking
// outright, mirroring the low/medium/high/off intensity picker Claude
// Code offers instead of a plain on/off toggle.
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
// thinking via a numeric token budget (ThinkingBudgetLevels) rather
// than catwalk-supplied named reasoning-effort levels.
func UsesThinkingBudget(providerType catwalk.Type) bool {
	return providerType == catwalk.TypeAnthropic || providerType == catwalk.TypeBedrock
}
