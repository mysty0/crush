package config

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveToken returns a token that is nowhere near expiry, so code paths
// under test never attempt a network refresh.
func liveToken(access string) *oauth.Token {
	return &oauth.Token{
		AccessToken:  access,
		RefreshToken: "refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
}

func claudeTestConfig() *Config {
	return &Config{Providers: csync.NewMap[string, ProviderConfig]()}
}

// TestClaudeCodeAccounts covers which Claude subscription providers are
// reported as configured accounts: named accounts need a token, the
// default account does not (it can authenticate from the Claude Code
// CLI's credentials file), and disabled providers are excluded.
func TestClaudeCodeAccounts(t *testing.T) {
	t.Parallel()

	t.Run("default account without a token counts", func(t *testing.T) {
		t.Parallel()
		cfg := claudeTestConfig()
		cfg.Providers.Set(claudecode.ProviderID, ProviderConfig{})
		assert.Equal(t, []string{claudecode.ProviderID}, cfg.ClaudeCodeAccounts())
	})

	t.Run("named account needs a token", func(t *testing.T) {
		t.Parallel()
		cfg := claudeTestConfig()
		cfg.Providers.Set("claude-code-work", ProviderConfig{})
		assert.Empty(t, cfg.ClaudeCodeAccounts())

		cfg.Providers.Set("claude-code-work", ProviderConfig{OAuthToken: liveToken("tok")})
		assert.Equal(t, []string{"claude-code-work"}, cfg.ClaudeCodeAccounts())
	})

	t.Run("disabled accounts are excluded", func(t *testing.T) {
		t.Parallel()
		cfg := claudeTestConfig()
		cfg.Providers.Set(claudecode.ProviderID, ProviderConfig{Disable: true})
		cfg.Providers.Set("claude-code-work", ProviderConfig{OAuthToken: liveToken("tok"), Disable: true})
		assert.Empty(t, cfg.ClaudeCodeAccounts())
	})

	t.Run("unrelated providers are excluded", func(t *testing.T) {
		t.Parallel()
		cfg := claudeTestConfig()
		cfg.Providers.Set("anthropic", ProviderConfig{OAuthToken: liveToken("tok")})
		assert.Empty(t, cfg.ClaudeCodeAccounts())
	})
}

// TestSeedClaudeCodeAccounts checks that a logged-in subscription account
// is turned into a fully configured Anthropic provider. Without this the
// account would be dropped at load time as a custom provider with no
// endpoint, type, or models.
func TestSeedClaudeCodeAccounts(t *testing.T) {
	t.Parallel()

	cfg := claudeTestConfig()
	// Models are pre-populated so seeding does not query /v1/models: model
	// discovery is exercised against a live endpoint elsewhere.
	cfg.Providers.Set("claude-code-work", ProviderConfig{
		OAuthToken: liveToken("tok"),
		OAuthExtra: map[string]string{"email": "me@example.com"},
		Models:     []catwalk.Model{{ID: "claude-opus-4-8"}},
		// Set by the user in crush.json; seeding must not clobber it.
		FlatRate: true,
	})
	store := NewTestStore(cfg)

	disableDiscovery := false
	cfg.seedClaudeCodeAccounts(context.Background(), store, &disableDiscovery)

	pc, ok := cfg.Providers.Get("claude-code-work")
	require.True(t, ok)
	assert.Equal(t, "claude-code-work", pc.ID)
	assert.Equal(t, catwalk.TypeAnthropic, pc.Type)
	assert.Equal(t, claudecode.BaseURL, pc.BaseURL)
	assert.Equal(t, "Claude Code (work)", pc.Name)
	assert.True(t, pc.FlatRate, "user-set fields survive seeding")
	require.NotNil(t, pc.AutoDiscoverModels)
	assert.False(t, *pc.AutoDiscoverModels, "the account's model list is authoritative")
}

// TestSeedClaudeCodeAccountsSkipsTokenless makes sure the default account
// backed by the Claude Code CLI credentials file is left alone: it is
// seeded from the file path in load.go instead.
func TestSeedClaudeCodeAccountsSkipsTokenless(t *testing.T) {
	t.Parallel()

	cfg := claudeTestConfig()
	cfg.Providers.Set(claudecode.ProviderID, ProviderConfig{})
	store := NewTestStore(cfg)

	disableDiscovery := false
	cfg.seedClaudeCodeAccounts(context.Background(), store, &disableDiscovery)

	pc, ok := cfg.Providers.Get(claudecode.ProviderID)
	require.True(t, ok)
	assert.Empty(t, pc.BaseURL)
	assert.Empty(t, pc.Type)
}

// TestClaudeCodeAccountName covers the label shown in the model picker.
func TestClaudeCodeAccountName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Claude Code", claudeCodeAccountName(claudecode.ProviderID, ProviderConfig{}))
	assert.Equal(t, "Claude Code (work)", claudeCodeAccountName("claude-code-work", ProviderConfig{}))
	assert.Equal(t, "Claude Code (me@example.com)", claudeCodeAccountName(
		claudecode.ProviderID,
		ProviderConfig{OAuthExtra: map[string]string{"email": "me@example.com"}},
	))
}

// TestStoreTokenSource covers the token source backing runtime requests
// for a logged-in account: a live token is handed over as is, and a
// missing account is an error rather than a silent fallback to another
// account's credentials.
func TestStoreTokenSource(t *testing.T) {
	t.Parallel()

	cfg := claudeTestConfig()
	cfg.Providers.Set("claude-code-work", ProviderConfig{OAuthToken: liveToken("live-token")})
	store := NewTestStore(cfg)

	token, err := storeTokenSource{store: store, providerID: "claude-code-work"}.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "live-token", token)

	_, err = storeTokenSource{store: store, providerID: "claude-code-missing"}.Token(context.Background())
	require.Error(t, err)
}

// TestClaudeCodeSourceSelection checks that each account resolves to its
// own token: a logged-in account uses its stored token rather than the
// shared credentials-file source every account would otherwise share.
func TestClaudeCodeSourceSelection(t *testing.T) {
	t.Parallel()

	cfg := claudeTestConfig()
	cfg.Providers.Set(claudecode.ProviderID, ProviderConfig{})
	cfg.Providers.Set("claude-code-work", ProviderConfig{OAuthToken: liveToken("work-token")})
	store := NewTestStore(cfg)

	work, err := store.ClaudeCodeSource("claude-code-work").Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "work-token", work)

	// The tokenless default account falls back to the credentials-file
	// source, which is shared process-wide.
	assert.Same(t, claudecode.DefaultSource(), store.ClaudeCodeSource(claudecode.ProviderID))
}
