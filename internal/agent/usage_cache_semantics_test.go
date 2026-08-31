package agent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// stubModel is a fantasy.LanguageModel that only reports a provider
// name, which is all the usage-accounting code reads off a model.
type stubModel struct {
	fantasy.LanguageModel
	provider string
}

func (m stubModel) Provider() string { return m.provider }
func (m stubModel) Model() string    { return "stub" }

// TestPromptTokensCacheSemantics covers the two conventions providers use
// to report cached prompt tokens.
//
// Anthropic-style providers report cache reads and writes as buckets
// *disjoint* from input tokens, so the prompt is their sum. Gemini
// reports its cached count as a *subset* of the prompt count, so summing
// counts the cached prefix twice -- which inflated the context gauge
// enough to trigger auto-compaction at roughly a fifth of a 1M window.
func TestPromptTokensCacheSemantics(t *testing.T) {
	t.Parallel()

	// Figures taken from a real Gemini turn: the cached prefix is most
	// of the prompt, so double counting nearly doubles the total.
	usage := fantasy.Usage{InputTokens: 51925, CacheReadTokens: 48400}

	require.Equal(t, int64(51925), promptTokens(usage, true),
		"cache read is already inside the input count and must not be added again")
	require.Equal(t, int64(100325), promptTokens(usage, false),
		"disjoint providers must still sum every prompt bucket")

	// Cache creation is only ever reported by disjoint-bucket providers,
	// and must keep counting toward the prompt there.
	withCreation := fantasy.Usage{InputTokens: 10, CacheCreationTokens: 90}
	require.Equal(t, int64(100), promptTokens(withCreation, false))
}

// TestNormalizeUsageClampsAccumulatedCacheReads guards the streaming
// accumulation fault: fantasy's google provider adds CacheReadTokens once
// per streamed chunk, but Gemini repeats a cumulative usage block in every
// chunk, so the figure ends up multiplied by the chunk count. The observed
// case reported 1.24M cache reads against a 113k prompt, pushing a session
// past a 1M context window and forcing compaction at ~22% real use.
func TestNormalizeUsageClampsAccumulatedCacheReads(t *testing.T) {
	t.Parallel()

	inflated := fantasy.Usage{InputTokens: 113420, CacheReadTokens: 1243342}
	got := normalizeUsage(inflated, true)
	require.Equal(t, int64(113420), got.CacheReadTokens,
		"the cached prefix is part of the prompt and cannot exceed it")
	require.Equal(t, int64(113420), got.InputTokens, "input tokens must be left alone")

	// A healthy reading is untouched.
	healthy := fantasy.Usage{InputTokens: 51925, CacheReadTokens: 48400}
	require.Equal(t, healthy, normalizeUsage(healthy, true))

	// Disjoint-bucket providers legitimately report cache reads larger
	// than their uncached input, so they must never be clamped.
	anthropicShaped := fantasy.Usage{InputTokens: 12, CacheReadTokens: 200000}
	require.Equal(t, anthropicShaped, normalizeUsage(anthropicShaped, false))
}

func TestCacheReadIsSubsetOfInput(t *testing.T) {
	t.Parallel()

	require.True(t, cacheReadIsSubsetOfInput("google"),
		"every provider routed through fantasy's google provider reports Gemini-shaped usage")
	require.False(t, cacheReadIsSubsetOfInput("anthropic"))
	require.False(t, cacheReadIsSubsetOfInput("openai"))

	// A model with no provider must not be assumed Gemini-shaped.
	require.False(t, modelCacheReadIsSubsetOfInput(Model{}))
	require.True(t, modelCacheReadIsSubsetOfInput(Model{Model: stubModel{provider: "google"}}))
	require.False(t, modelCacheReadIsSubsetOfInput(Model{Model: stubModel{provider: "anthropic"}}))
}

// TestUpdateSessionUsageGeminiDoesNotDoubleCountCache is the end-to-end
// regression: a Gemini turn must record a context size equal to its real
// prompt, not the sum of prompt and its own cached subset.
func TestUpdateSessionUsageGeminiDoesNotDoubleCountCache(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	sess := &session.Session{ID: "s"}
	model := Model{Model: stubModel{provider: "google"}}

	// The inflated cache figure a chunked Gemini stream produces.
	agent.updateSessionUsage(model, sess, fantasy.Usage{
		InputTokens:     113420,
		OutputTokens:    1700,
		CacheReadTokens: 1243342,
	}, nil, false)

	require.Equal(t, int64(113420), sess.PromptTokens,
		"the context gauge must reflect the real prompt, not prompt+cache+chunk multiplier")
	require.Equal(t, int64(1700), sess.CompletionTokens)
	require.LessOrEqual(t, sess.CacheReadTokens, sess.PromptTokens,
		"the recorded cached prefix must stay within the prompt")
}
