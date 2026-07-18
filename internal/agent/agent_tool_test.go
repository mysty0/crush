package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIsolatedTestCoordinator builds on newTestCoordinator but also strips
// any provider/model config.Init may have picked up from the real
// environment (e.g. a live Claude Code subscription on the dev machine),
// so tests that inspect EnabledProviders() or the large/small model slots
// only ever see the one provider under test.
func newIsolatedTestCoordinator(t *testing.T, env fakeEnv, providerID string, providerCfg config.ProviderConfig) *coordinator {
	t.Helper()
	coord := newTestCoordinator(t, env, providerID, providerCfg)
	for id, pc := range coord.cfg.Config().Providers.Seq2() {
		if id == providerID {
			continue
		}
		pc.Disable = true
		coord.cfg.Config().Providers.Set(id, pc)
	}
	clear(coord.cfg.Config().Models)
	return coord
}

func TestResolveDefaultSonnetModel(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"

	t.Run("picks a sonnet model from an enabled provider", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		providerCfg := config.ProviderConfig{
			ID: providerID,
			Models: []catwalk.Model{
				{ID: "claude-opus-4-8", Name: "Claude Opus 4.8"},
				{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", DefaultMaxTokens: 128000},
				{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5"},
			},
		}
		coord := newIsolatedTestCoordinator(t, env, providerID, providerCfg)

		selected, ok := coord.resolveDefaultSonnetModel()
		require.True(t, ok)
		assert.Equal(t, providerID, selected.Provider)
		assert.Equal(t, "claude-sonnet-4-6", selected.Model)
		assert.Equal(t, int64(128000), selected.MaxTokens)
	})

	t.Run("matches by display name when the ID doesn't say sonnet", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		providerCfg := config.ProviderConfig{
			ID: providerID,
			Models: []catwalk.Model{
				{ID: "claude-3-5-20241022", Name: "Claude 3.5 Sonnet"},
			},
		}
		coord := newIsolatedTestCoordinator(t, env, providerID, providerCfg)

		selected, ok := coord.resolveDefaultSonnetModel()
		require.True(t, ok)
		assert.Equal(t, "claude-3-5-20241022", selected.Model)
	})

	t.Run("no sonnet model on any enabled provider", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		providerCfg := config.ProviderConfig{
			ID: providerID,
			Models: []catwalk.Model{
				{ID: "gpt-5", Name: "GPT-5"},
			},
		}
		coord := newIsolatedTestCoordinator(t, env, providerID, providerCfg)

		_, ok := coord.resolveDefaultSonnetModel()
		assert.False(t, ok)
	})
}

func TestResolveFetchModelSelection(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{
		ID: providerID,
		Models: []catwalk.Model{
			{ID: "claude-opus-4-8", Name: "Claude Opus 4.8"},
			{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
		},
	}

	t.Run("defaults to sonnet when no model is given", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord := newIsolatedTestCoordinator(t, env, providerID, providerCfg)

		selected, err := coord.resolveFetchModelSelection("")
		require.NoError(t, err)
		assert.Equal(t, "claude-sonnet-4-6", selected.Model)
	})

	t.Run("honors an explicit model ID", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord := newIsolatedTestCoordinator(t, env, providerID, providerCfg)

		selected, err := coord.resolveFetchModelSelection("claude-opus-4-8")
		require.NoError(t, err)
		assert.Equal(t, "claude-opus-4-8", selected.Model)
	})

	t.Run("rejects an unknown model ID", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord := newIsolatedTestCoordinator(t, env, providerID, providerCfg)

		_, err := coord.resolveFetchModelSelection("no-such-model")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no-such-model")
	})

	t.Run("falls back to the configured small model when no sonnet is available", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		noSonnetProviderCfg := config.ProviderConfig{
			ID: providerID,
			Models: []catwalk.Model{
				{ID: "gpt-5", Name: "GPT-5"},
			},
		}
		coord := newIsolatedTestCoordinator(t, env, providerID, noSonnetProviderCfg)
		coord.cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
			Provider: providerID,
			Model:    "gpt-5",
		}

		selected, err := coord.resolveFetchModelSelection("")
		require.NoError(t, err)
		assert.Equal(t, "gpt-5", selected.Model)
	})

	t.Run("errors when no sonnet is available and no small model is configured", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		noSonnetProviderCfg := config.ProviderConfig{
			ID: providerID,
			Models: []catwalk.Model{
				{ID: "gpt-5", Name: "GPT-5"},
			},
		}
		coord := newIsolatedTestCoordinator(t, env, providerID, noSonnetProviderCfg)

		_, err := coord.resolveFetchModelSelection("")
		require.Error(t, err)
	})
}
