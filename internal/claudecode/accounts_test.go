package claudecode

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsProviderID pins which provider ids activate native Claude Code
// subscription handling. It must match the default provider and every
// named account, and must not swallow unrelated providers that merely
// start with "claude".
func TestIsProviderID(t *testing.T) {
	t.Parallel()

	for _, id := range []string{ProviderID, "claude-code-work", "claude-code-a-b"} {
		assert.True(t, IsProviderID(id), id)
	}
	for _, id := range []string{"", "anthropic", "claude", "claude-codex", "not-claude-code"} {
		assert.False(t, IsProviderID(id), id)
	}
}

// TestProviderIDForAccount covers the account-name-to-provider-id mapping
// that keeps every subscription in its own provider entry. The unnamed
// (and explicitly "default") account keeps the original provider id so
// existing configs are untouched.
func TestProviderIDForAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		account string
		want    string
	}{
		{"", ProviderID},
		{"   ", ProviderID},
		{"default", ProviderID},
		{"work", "claude-code-work"},
		{"Work", "claude-code-work"},
		{"  work  ", "claude-code-work"},
		{"work account", "claude-code-work-account"},
		{"work_account!", "claude-code-work-account"},
		{"me@example.com", "claude-code-me-example-com"},
		{"!!!", ProviderID},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, ProviderIDForAccount(tt.account), tt.account)
	}
}

// TestAccountFromProviderID covers the inverse mapping used for display
// labels and logout.
func TestAccountFromProviderID(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "", AccountFromProviderID(ProviderID))
	assert.Equal(t, "", AccountFromProviderID("anthropic"))
	assert.Equal(t, "work", AccountFromProviderID("claude-code-work"))
	assert.Equal(t, "work-account", AccountFromProviderID("claude-code-work-account"))

	// Round-trips for every account name that maps to a named provider.
	for _, account := range []string{"work", "personal", "team two"} {
		id := ProviderIDForAccount(account)
		assert.Equal(t, AccountSlug(account), AccountFromProviderID(id), account)
	}
}

// TestSplitManualCode covers parsing the "code#state" value the hosted
// success page shows in the headless login flow.
func TestSplitManualCode(t *testing.T) {
	t.Parallel()

	code, state := splitManualCode("abc#xyz")
	assert.Equal(t, "abc", code)
	assert.Equal(t, "xyz", state)

	code, state = splitManualCode("  abc#xyz  ")
	assert.Equal(t, "abc", code)
	assert.Equal(t, "xyz", state)

	// A bare code (no fragment) is accepted; there is just no state to
	// cross-check against.
	code, state = splitManualCode("abc")
	assert.Equal(t, "abc", code)
	assert.Equal(t, "", state)

	code, _ = splitManualCode("")
	assert.Equal(t, "", code)
}
