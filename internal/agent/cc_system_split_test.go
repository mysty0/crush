package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// decodeSystemBlocks decodes the system field of a rewritten body into a
// slice of {type,text} blocks for assertions. It fails the test if the
// system field is not an array of text blocks.
func decodeSystemBlocks(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &payload))
	raw, ok := payload["system"]
	require.True(t, ok, "body has no system field")
	var blocks []map[string]any
	require.NoError(t, json.Unmarshal(raw, &blocks))
	return blocks
}

func TestCCEnsureIdentityFirst_SplitsSoleBlockStartingWithIdentity(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"m","system":"` + ccClaudeCodeIdentity + `\n\nYou are great.","messages":[]}`)

	out, changed := ccEnsureIdentityFirst(body, false)
	require.True(t, changed)

	blocks := decodeSystemBlocks(t, out)
	require.Len(t, blocks, 2)
	require.Equal(t, ccClaudeCodeIdentity, blocks[0]["text"])
	require.Equal(t, "You are great.", blocks[1]["text"])

	// Other fields preserved.
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &payload))
	require.Contains(t, string(payload["model"]), "m")
}

func TestCCEnsureIdentityFirst_InjectsWhenAbsentString(t *testing.T) {
	t.Parallel()
	// A title-style request: a single system string with no identity.
	body := []byte(`{"system":"You will generate a short title.","messages":[]}`)

	// Without injection this path is a no-op (plain API-key traffic).
	_, changed := ccEnsureIdentityFirst(body, false)
	require.False(t, changed)

	// With injection the identity is prepended as the first block.
	out, changed := ccEnsureIdentityFirst(body, true)
	require.True(t, changed)

	blocks := decodeSystemBlocks(t, out)
	require.Len(t, blocks, 2)
	require.Equal(t, ccClaudeCodeIdentity, blocks[0]["text"])
	require.Equal(t, "You will generate a short title.", blocks[1]["text"])
}

func TestCCEnsureIdentityFirst_InjectsWhenAbsentMultiBlock(t *testing.T) {
	t.Parallel()
	// A summary/sub-agent request whose system is already an array of
	// blocks, none of which is the identity.
	body := []byte(`{"system":[{"type":"text","text":"prefix"},{"type":"text","text":"body"}],"messages":[]}`)

	out, changed := ccEnsureIdentityFirst(body, true)
	require.True(t, changed)

	blocks := decodeSystemBlocks(t, out)
	require.Len(t, blocks, 3)
	require.Equal(t, ccClaudeCodeIdentity, blocks[0]["text"])
	require.Equal(t, "prefix", blocks[1]["text"])
	require.Equal(t, "body", blocks[2]["text"])
}

func TestCCEnsureIdentityFirst_InjectsWhenNoSystemField(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"m","messages":[]}`)

	_, changed := ccEnsureIdentityFirst(body, false)
	require.False(t, changed)

	out, changed := ccEnsureIdentityFirst(body, true)
	require.True(t, changed)

	blocks := decodeSystemBlocks(t, out)
	require.Len(t, blocks, 1)
	require.Equal(t, ccClaudeCodeIdentity, blocks[0]["text"])
}

func TestCCEnsureIdentityFirst_IdempotentWhenIdentityIsFirstBlock(t *testing.T) {
	t.Parallel()
	body := []byte(`{"system":[{"type":"text","text":"` + ccClaudeCodeIdentity + `"},{"type":"text","text":"rest"}],"messages":[]}`)

	// Already correct: no change, regardless of injection.
	_, changed := ccEnsureIdentityFirst(body, true)
	require.False(t, changed)
	_, changed = ccEnsureIdentityFirst(body, false)
	require.False(t, changed)
}

func TestCCEnsureIdentityFirst_SplitThenIdempotent(t *testing.T) {
	t.Parallel()
	body := []byte(`{"system":"` + ccClaudeCodeIdentity + `\nrest","messages":[]}`)

	out, changed := ccEnsureIdentityFirst(body, true)
	require.True(t, changed)

	// Feeding the rewritten body back is a no-op.
	_, changed = ccEnsureIdentityFirst(out, true)
	require.False(t, changed)
}

func TestCCEnsureIdentityFirst_PreservesCacheControlOnSplit(t *testing.T) {
	t.Parallel()
	body := []byte(`{"system":[{"type":"text","text":"` + ccClaudeCodeIdentity + `\nrest","cache_control":{"type":"ephemeral"}}],"messages":[]}`)

	out, changed := ccEnsureIdentityFirst(body, true)
	require.True(t, changed)

	blocks := decodeSystemBlocks(t, out)
	require.Len(t, blocks, 2)
	require.Equal(t, ccClaudeCodeIdentity, blocks[0]["text"])
	require.Equal(t, "rest", blocks[1]["text"])
	// Cache breakpoint stays on the bulk block, not the identity.
	require.Nil(t, blocks[0]["cache_control"])
	require.NotNil(t, blocks[1]["cache_control"])
}

func TestCCEnsureIdentityFirst_NoInjectLeavesNonIdentityAlone(t *testing.T) {
	t.Parallel()
	// Plain Anthropic API-key traffic: a normal system prompt that does not
	// start with the identity must be left untouched when inject is off.
	body := []byte(`{"system":"You are a helpful assistant.","messages":[]}`)

	_, changed := ccEnsureIdentityFirst(body, false)
	require.False(t, changed)
}
