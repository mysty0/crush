package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefreshOAuthProviderBeforeModelDiscovery_ExpiredTokenRefreshes(t *testing.T) {
	t.Parallel()

	expired := &oauth.Token{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}
	fresh := &oauth.Token{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}

	configPath := filepath.Join(t.TempDir(), "crush.json")
	configContent := fmt.Sprintf(`{
		"providers": {
			"%s": {
				"api_key": "old-access",
				"oauth": {
					"access_token": "old-access",
					"refresh_token": "old-refresh",
					"expires_in": 3600,
					"expires_at": %d
				}
			}
		}
	}`, antigravity.ProviderID, expired.ExpiresAt)
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(antigravity.ProviderID, ProviderConfig{
		ID:         antigravity.ProviderID,
		Name:       "Google Antigravity",
		APIKey:     expired.AccessToken,
		OAuthToken: expired,
	})
	cfg := &Config{Providers: providers}
	store := &ConfigStore{
		config:         cfg,
		globalDataPath: configPath,
		exchangeToken: func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error) {
			require.Equal(t, antigravity.ProviderID, providerID)
			require.Equal(t, "old-refresh", refreshToken)
			return fresh, nil
		},
	}

	pc, ok := cfg.Providers.Get(antigravity.ProviderID)
	require.True(t, ok)
	pc = cfg.refreshOAuthProviderBeforeModelDiscovery(context.Background(), store, antigravity.ProviderID, pc)

	require.Equal(t, "new-access", pc.OAuthToken.AccessToken)
	require.Equal(t, "new-access", pc.APIKey)
	require.Empty(t, cfg.OAuthModelWarnings)

	diskToken, err := store.loadTokenFromDisk(ScopeGlobal, antigravity.ProviderID)
	require.NoError(t, err)
	require.NotNil(t, diskToken)
	require.Equal(t, "new-access", diskToken.AccessToken)
}

func TestRefreshOAuthProviderBeforeModelDiscovery_RefreshFailureWarns(t *testing.T) {
	t.Parallel()

	expired := &oauth.Token{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}

	configPath := filepath.Join(t.TempDir(), "crush.json")
	configContent := fmt.Sprintf(`{
		"providers": {
			"%s": {
				"api_key": "old-access",
				"oauth": {
					"access_token": "old-access",
					"refresh_token": "old-refresh",
					"expires_in": 3600,
					"expires_at": %d
				}
			}
		}
	}`, antigravity.ProviderID, expired.ExpiresAt)
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(antigravity.ProviderID, ProviderConfig{
		ID:         antigravity.ProviderID,
		Name:       "Google Antigravity",
		APIKey:     expired.AccessToken,
		OAuthToken: expired,
	})
	cfg := &Config{Providers: providers}
	store := &ConfigStore{
		config:         cfg,
		globalDataPath: configPath,
		exchangeToken: func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error) {
			return nil, fmt.Errorf("refresh failed")
		},
	}

	pc, ok := cfg.Providers.Get(antigravity.ProviderID)
	require.True(t, ok)
	pc = cfg.refreshOAuthProviderBeforeModelDiscovery(context.Background(), store, antigravity.ProviderID, pc)

	require.Equal(t, "old-access", pc.OAuthToken.AccessToken)
	require.Len(t, cfg.OAuthModelWarnings, 1)
	require.Contains(t, cfg.OAuthModelWarnings[0], "OAuth token refresh failed")
}

// TestIsSelectedModelProvider covers the check that decides whether an
// OAuth-subscription account's model discovery must be resolved
// synchronously (see seedModels): only the provider actually behind the
// user's currently configured large or small model should qualify.
func TestIsSelectedModelProvider(t *testing.T) {
	t.Parallel()

	cfg := &Config{Models: map[SelectedModelType]SelectedModel{
		SelectedModelTypeLarge: {Provider: "claude-code-work", Model: "claude-opus-5"},
		SelectedModelTypeSmall: {Provider: "claude-code", Model: "claude-opus-4-8"},
	}}

	assert.True(t, isSelectedModelProvider(cfg, "claude-code-work"), "the configured large model's provider")
	assert.True(t, isSelectedModelProvider(cfg, "claude-code"), "the configured small model's provider")
	assert.False(t, isSelectedModelProvider(cfg, "google-antigravity"), "a logged-in but unselected provider")

	assert.False(t, isSelectedModelProvider(&Config{}, "claude-code"), "nil Models map must not panic or match")
}

// TestSeedModelsResolvesSelectedProviderSynchronously is a regression
// test for a real bug: after model discovery for OAuth-subscription
// providers moved to the background to speed up startup, Load's model
// validation ran a few lines later against only the static fallback
// list -- which is missing anything released since this build -- so an
// already-valid selection (e.g. one picked in a previous session, once
// the account gained access to it) was falsely reported "unavailable"
// and silently swapped for an unrelated model, on every single launch.
//
// The fix: the provider actually behind the user's current selection
// is always resolved synchronously, so validation sees its real model
// list; every other logged-in account still defers to the background.
func TestSeedModelsResolvesSelectedProviderSynchronously(t *testing.T) {
	t.Parallel()

	t.Run("selected provider fetches synchronously", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "claude-code-work", Model: "claude-sonnet-5"},
		}}
		store := NewTestStore(cfg)
		fallback := []catwalk.Model{{ID: "claude-opus-4-8"}}
		live := []catwalk.Model{{ID: "claude-sonnet-5"}}

		var called bool
		got := seedModels(cfg, store, "claude-code-work", "Claude Code (work)", fallback,
			func(context.Context) ([]catwalk.Model, error) {
				called = true
				return live, nil
			})

		assert.True(t, called, "the selected provider's live list must be fetched before seedModels returns")
		assert.Equal(t, live, got, "the freshly fetched list must be used, not the stale fallback")
	})

	t.Run("selected provider fetch failure warns and falls back", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "claude-code-work", Model: "claude-sonnet-5"},
		}}
		store := NewTestStore(cfg)
		fallback := []catwalk.Model{{ID: "claude-opus-4-8"}}

		got := seedModels(cfg, store, "claude-code-work", "Claude Code (work)", fallback,
			func(context.Context) ([]catwalk.Model, error) {
				return nil, fmt.Errorf("network unreachable")
			})

		assert.Equal(t, fallback, got)
		require.Len(t, cfg.OAuthModelWarnings, 1)
		assert.Contains(t, cfg.OAuthModelWarnings[0], "Claude Code (work)")
	})

	t.Run("unselected provider defers to the background", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "claude-code-work", Model: "claude-sonnet-5"},
		}}
		store := NewTestStore(cfg)
		fallback := []catwalk.Model{{ID: "gemini-2.5-pro"}}

		var called bool
		got := seedModels(cfg, store, "google-antigravity", "Google Antigravity", fallback,
			func(context.Context) ([]catwalk.Model, error) {
				called = true
				return []catwalk.Model{{ID: "gemini-3.8-flash-tiered"}}, nil
			})

		// seedModelsInBackground short-circuits under testing.Testing(),
		// so fetch must not run at all here -- this only asserts the
		// branch taken, not the background path's own behavior (see
		// seedModelsInBackground's tests, if any, for that).
		assert.False(t, called, "an unselected provider must not be fetched synchronously")
		assert.Equal(t, fallback, got)
	})
}

// TestFetchModelsWithRetry is a regression test for a real bug: the
// background model-discovery fetch used to give up permanently after
// its single attempt failed, silently and with no retry, leaving the
// affected Crush process stuck on the static fallback model list for
// its entire lifetime -- e.g. a burst of many simultaneous launches
// (tmux restoring dozens of panes at once) transiently failing or
// getting rate-limited on the OAuth token refresh or model-list fetch.
func TestFetchModelsWithRetry(t *testing.T) {
	t.Parallel()

	// Delays are all zero so the test runs instantly regardless of how
	// many attempts it takes; only the number and outcome of attempts
	// matters here, not real wall-clock backoff (that is a fixed,
	// hardcoded schedule in seedModelsInBackgroundRetryDelays, not
	// worth re-asserting here).
	fastDelays := []time.Duration{0, 0, 0, 0, 0}

	t.Run("succeeds on a later attempt", func(t *testing.T) {
		t.Parallel()
		var attempts int
		want := []catwalk.Model{{ID: "claude-sonnet-5"}}

		got, err := fetchModelsWithRetry("claude-code-work", func(context.Context) ([]catwalk.Model, error) {
			attempts++
			if attempts < 3 {
				return nil, fmt.Errorf("rate limited")
			}
			return want, nil
		}, fastDelays)

		require.NoError(t, err)
		assert.Equal(t, want, got)
		assert.Equal(t, 3, attempts, "must stop retrying as soon as an attempt succeeds")
	})

	t.Run("gives up after exhausting every attempt", func(t *testing.T) {
		t.Parallel()
		var attempts int

		got, err := fetchModelsWithRetry("claude-code-work", func(context.Context) ([]catwalk.Model, error) {
			attempts++
			return nil, fmt.Errorf("network unreachable")
		}, fastDelays)

		require.Error(t, err)
		assert.Nil(t, got)
		assert.Equal(t, len(fastDelays), attempts, "must use every configured attempt before giving up")
	})

	t.Run("an empty model list on success is treated as a failure", func(t *testing.T) {
		t.Parallel()
		var attempts int

		got, err := fetchModelsWithRetry("claude-code-work", func(context.Context) ([]catwalk.Model, error) {
			attempts++
			return []catwalk.Model{}, nil
		}, fastDelays)

		require.Error(t, err, "a nil error with zero models is not a usable result")
		assert.Nil(t, got)
		assert.Equal(t, len(fastDelays), attempts)
	})
}
