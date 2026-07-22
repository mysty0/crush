package message

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTokenUsagePartRoundTrip verifies a TokenUsage part survives the same
// JSON marshal/unmarshal that persists messages to the DB, so per-turn usage
// can be read back from history.
func TestTokenUsagePartRoundTrip(t *testing.T) {
	t.Parallel()
	parts := []ContentPart{
		TextContent{Text: "hi"},
		TokenUsage{
			InputTokens:         12,
			OutputTokens:        34,
			CacheReadTokens:     5600,
			CacheCreationTokens: 78,
			Cost:                0.0123,
			Estimated:           true,
		},
	}

	data, err := marshalParts(parts)
	require.NoError(t, err)

	got, err := unmarshalParts(data)
	require.NoError(t, err)
	require.Len(t, got, 2)

	usage, ok := got[1].(TokenUsage)
	require.True(t, ok, "second part should decode back to TokenUsage")
	require.Equal(t, parts[1], usage)
}

// TestAddTokenUsageReplaces verifies AddTokenUsage keeps at most one usage
// part and TokenUsagePart reads it back.
func TestAddTokenUsageReplaces(t *testing.T) {
	t.Parallel()
	m := &Message{}
	require.Nil(t, m.TokenUsagePart())

	m.AddTokenUsage(TokenUsage{OutputTokens: 1})
	m.AddTokenUsage(TokenUsage{OutputTokens: 2})

	count := 0
	for _, p := range m.Parts {
		if _, ok := p.(TokenUsage); ok {
			count++
		}
	}
	require.Equal(t, 1, count, "only one usage part should remain")

	got := m.TokenUsagePart()
	require.NotNil(t, got)
	require.Equal(t, int64(2), got.OutputTokens)
}
