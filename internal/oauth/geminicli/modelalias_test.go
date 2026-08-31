package geminicli

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// fakeAliasProvider records the model id it was asked for, standing in
// for fantasy's google provider (which sniffs that id).
type fakeAliasProvider struct {
	gotModelID string
}

func (p *fakeAliasProvider) Name() string { return "fake" }

func (p *fakeAliasProvider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	p.gotModelID = modelID
	return &fakeAliasModel{modelID: modelID}, nil
}

type fakeAliasModel struct {
	fantasy.LanguageModel
	modelID string
}

func (m *fakeAliasModel) Model() string    { return m.modelID }
func (m *fakeAliasModel) Provider() string { return "fake" }

func TestAliasModelIDRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		modelID string
		aliased bool
	}{
		{"claude is aliased", "claude-sonnet-4-6", true},
		{"claude thinking variant is aliased", "claude-opus-4-6-thinking", true},
		{"anthropic substring is aliased", "some-anthropic-model", true},
		{"mixed case is aliased", "Claude-Sonnet-4-6", true},
		{"gemini is untouched", "gemini-3.6-flash-high", false},
		{"gpt is untouched", "gpt-oss-120b-medium", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			alias := aliasModelID(tc.modelID)

			if !tc.aliased {
				require.Equal(t, tc.modelID, alias, "model needs no alias")
			} else {
				require.NotEqual(t, tc.modelID, alias)
				// The whole point: the alias must not trip the
				// substring check in fantasy's google provider.
				require.False(t, needsModelAlias(alias),
					"alias must not itself look like an Anthropic model")
			}
			require.Equal(t, tc.modelID, unaliasModelID(alias),
				"unaliasing must recover the exact original id")
		})
	}
}

// TestUnaliasModelIDLeavesUnknownIDsAlone guards the reverse direction
// against corrupting a real model id, including one that merely starts
// with the alias prefix but is not valid hex.
func TestUnaliasModelIDLeavesUnknownIDsAlone(t *testing.T) {
	t.Parallel()

	for _, id := range []string{
		"gemini-3.6-flash-high",
		"claude-sonnet-4-6",
		modelAliasPrefix + "not-hex!",
		"",
	} {
		require.Equal(t, id, unaliasModelID(id))
	}
}

// TestWrapCodeAssistWireFormatHidesClaudeFromProvider is the regression
// that matters: the wrapped provider must never hand a Claude-looking
// model id to the inner provider, because fantasy's google provider
// diverts those to its Anthropic provider (dropping the base URL and
// producing a 404 against /v1/messages). Callers must still see the real
// id back.
func TestWrapCodeAssistWireFormatHidesClaudeFromProvider(t *testing.T) {
	t.Parallel()

	inner := &fakeAliasProvider{}
	p := WrapCodeAssistWireFormat(inner)

	m, err := p.LanguageModel(context.Background(), "claude-sonnet-4-6")
	require.NoError(t, err)

	require.NotContains(t, inner.gotModelID, "claude",
		"the inner provider must not see a Claude-looking id, or it will divert the model")
	require.Equal(t, "claude-sonnet-4-6", m.Model(),
		"callers must still observe the real model id")
	require.Equal(t, "claude-sonnet-4-6", unaliasModelID(inner.gotModelID),
		"the wire must be able to recover the real id from the alias")
}

// TestWrapCodeAssistWireFormatPassesGeminiThrough confirms the wrapper is
// inert for models fantasy would not have diverted, so the common path
// is untouched (and not needlessly wrapped).
func TestWrapCodeAssistWireFormatPassesGeminiThrough(t *testing.T) {
	t.Parallel()

	inner := &fakeAliasProvider{}
	p := WrapCodeAssistWireFormat(inner)

	m, err := p.LanguageModel(context.Background(), "gemini-3.6-flash-high")
	require.NoError(t, err)

	require.Equal(t, "gemini-3.6-flash-high", inner.gotModelID)
	require.Equal(t, "gemini-3.6-flash-high", m.Model())
}
