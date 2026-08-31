package geminicli

import (
	"context"
	"encoding/hex"
	"strings"

	"charm.land/fantasy"
)

// WrapCodeAssistWireFormat keeps every Cloud Code Assist model on the
// Code Assist wire path, including the Claude and GPT models the backend
// offers alongside Gemini.
//
// Those models need no special handling: capturing a real Claude turn
// from the Antigravity CLI shows the backend serves them from the same
// v1internal:streamGenerateContent endpoint, in the same request
// envelope, in Gemini's own wire vocabulary -- contents/parts,
// systemInstruction, tools.functionDeclarations on the way out, and
// candidates with parts[].thought, parts[].functionCall and
// finishReason on the way back. The only Anthropic trace is cosmetic
// (modelVersion "claude-sonnet-4-6", tool ids prefixed "toolu_vrtx_").
// Google runs its own Anthropic conversion server-side; clients only
// ever speak Gemini.
//
// fantasy's google provider does not know that. It sniffs the model name
// (providers/google/google.go: strings.Contains(modelID, "anthropic") ||
// strings.Contains(modelID, "claude")) and hands any match to its
// Anthropic provider, which emits POST {host}/v1/messages -- a path this
// backend does not serve, and which WireTransport does not recognize as
// a generate call, so it is forwarded verbatim and 404s. Worse, that
// branch drops the configured base URL entirely, so the request does not
// even reach the right host.
//
// Renaming the model on the way in is what avoids that branch. The
// alias is only ever seen by fantasy and by genai's URL builder, both of
// which treat it as an opaque string; WireTransport restores the real id
// before it reaches the wire (see unaliasModelID), and Model() reports
// the real id back to Crush.
func WrapCodeAssistWireFormat(p fantasy.Provider) fantasy.Provider {
	return &codeAssistProvider{Provider: p}
}

type codeAssistProvider struct {
	fantasy.Provider
}

func (p *codeAssistProvider) LanguageModel(ctx context.Context, modelID string) (fantasy.LanguageModel, error) {
	alias := aliasModelID(modelID)
	m, err := p.Provider.LanguageModel(ctx, alias)
	if err != nil {
		return nil, err
	}
	if alias == modelID {
		return m, nil
	}
	return &aliasedModel{LanguageModel: m, realID: modelID}, nil
}

// aliasedModel restores the real model id for callers, so an alias never
// escapes into Crush's config, logs, or UI.
type aliasedModel struct {
	fantasy.LanguageModel
	realID string
}

func (m *aliasedModel) Model() string { return m.realID }

// modelAliasPrefix marks a model id rewritten by aliasModelID. It is
// deliberately not a plausible real model name so unaliasModelID cannot
// mistake a genuine id for an alias.
const modelAliasPrefix = "ccawire-"

// needsModelAlias reports whether fantasy's google provider would divert
// this model to its Anthropic provider. It matches case-insensitively:
// fantasy's own check is case-sensitive over lowercase substrings, so
// this is deliberately the wider net.
func needsModelAlias(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic")
}

// aliasModelID rewrites a model id that would otherwise be diverted, and
// returns every other id unchanged.
//
// The encoding is hex rather than a lookup table so the mapping is
// stateless and exactly reversible: nothing has to be registered up
// front, and a model the catalog gains later needs no code change. Hex
// also cannot reintroduce the sniffed substrings, and stays safe in a
// URL path.
func aliasModelID(modelID string) string {
	if !needsModelAlias(modelID) {
		return modelID
	}
	return modelAliasPrefix + hex.EncodeToString([]byte(modelID))
}

// unaliasModelID reverses aliasModelID. Anything that is not a
// well-formed alias is returned unchanged, so a real model id that
// happens to start with the prefix cannot be corrupted.
func unaliasModelID(modelID string) string {
	rest, ok := strings.CutPrefix(modelID, modelAliasPrefix)
	if !ok {
		return modelID
	}
	raw, err := hex.DecodeString(rest)
	if err != nil {
		return modelID
	}
	return string(raw)
}
