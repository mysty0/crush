package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

// inlineTestConfig builds a Config with two providers whose model lists
// are ordered newest-first, mirroring how catwalk and the OAuth
// subscription providers curate them.
func inlineTestConfig() *Config {
	return &Config{
		Options: &Options{},
		Models:  map[SelectedModelType]SelectedModel{},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			claudecode.ProviderID: {
				ID: claudecode.ProviderID,
				Models: []catwalk.Model{
					{ID: "claude-opus-5"},
					{ID: "claude-sonnet-5"},
					{ID: "claude-opus-4-8"},
					{ID: "claude-sonnet-4-6"},
				},
			},
			"yunwu": {
				ID: "yunwu",
				Models: []catwalk.Model{
					{ID: "claude-opus-4-8"},
					{ID: "claude-sonnet-4-6"},
					{ID: "deepseek-v3.2"},
				},
			},
		}),
	}
}

func TestResolveInlineModels(t *testing.T) {
	t.Parallel()

	t.Run("first match in provider order wins", func(t *testing.T) {
		t.Parallel()
		cfg := inlineTestConfig()
		cfg.Options.AgentModels = &AgentModelsOptions{
			Inline: []InlineModelRef{
				{Source: claudecode.ProviderID, Match: "*opus*"},
				{Source: claudecode.ProviderID, Match: "*sonnet*"},
			},
		}

		models, warnings := cfg.ResolveInlineModels()

		require.Empty(t, warnings)
		require.Len(t, models, 2)
		// Newest-first ordering means the flagship wins, not the
		// older model that also matches.
		require.Equal(t, "claude-opus-5", models[0].Model)
		require.Equal(t, "claude-sonnet-5", models[1].Model)
	})

	t.Run("source scopes matching to one provider", func(t *testing.T) {
		t.Parallel()
		cfg := inlineTestConfig()
		cfg.Options.AgentModels = &AgentModelsOptions{
			Inline: []InlineModelRef{{Source: "yunwu", Match: "*opus*"}},
		}

		models, warnings := cfg.ResolveInlineModels()

		require.Empty(t, warnings)
		require.Len(t, models, 1)
		require.Equal(t, "yunwu", models[0].Provider)
		// claude-opus-4-8 exists under both providers; the source
		// restriction decides which one is advertised.
		require.Equal(t, "claude-opus-4-8", models[0].Model)
	})

	t.Run("claude-code source covers named accounts", func(t *testing.T) {
		t.Parallel()
		cfg := inlineTestConfig()
		cfg.Providers.Set("claude-code-work", ProviderConfig{
			ID:     "claude-code-work",
			Models: []catwalk.Model{{ID: "claude-fable-5"}},
		})
		cfg.Options.AgentModels = &AgentModelsOptions{
			Inline: []InlineModelRef{{Source: claudecode.ProviderID, Match: "*fable*"}},
		}

		models, warnings := cfg.ResolveInlineModels()

		require.Empty(t, warnings)
		require.Len(t, models, 1)
		require.Equal(t, "claude-code-work", models[0].Provider)
	})

	t.Run("unmatched pattern warns without failing", func(t *testing.T) {
		t.Parallel()
		cfg := inlineTestConfig()
		cfg.Options.AgentModels = &AgentModelsOptions{
			Inline: []InlineModelRef{
				{Source: claudecode.ProviderID, Match: "*opus*"},
				{Source: claudecode.ProviderID, Match: "*nonexistent*"},
			},
		}

		models, warnings := cfg.ResolveInlineModels()

		require.Len(t, models, 1)
		require.Len(t, warnings, 1)
		require.Contains(t, warnings[0], "*nonexistent*")
		require.Contains(t, warnings[0], "list_models")
	})

	t.Run("configured slots are always included and deduped", func(t *testing.T) {
		t.Parallel()
		cfg := inlineTestConfig()
		cfg.Models[SelectedModelTypeLarge] = SelectedModel{
			Provider: claudecode.ProviderID, Model: "claude-opus-5",
		}
		cfg.Models[SelectedModelTypeSmall] = SelectedModel{
			Provider: claudecode.ProviderID, Model: "claude-sonnet-4-6",
		}
		cfg.Options.AgentModels = &AgentModelsOptions{
			// Resolves to claude-opus-5, already present as the
			// large slot, so it must not be listed twice.
			Inline: []InlineModelRef{{Source: claudecode.ProviderID, Match: "*opus*"}},
		}

		models, warnings := cfg.ResolveInlineModels()

		require.Empty(t, warnings)
		require.Len(t, models, 2)
		require.Equal(t, "claude-opus-5", models[0].Model)
		require.Equal(t, "claude-sonnet-4-6", models[1].Model)
	})

	t.Run("no config falls back to the configured slots", func(t *testing.T) {
		t.Parallel()
		cfg := inlineTestConfig()
		cfg.Models[SelectedModelTypeLarge] = SelectedModel{
			Provider: claudecode.ProviderID, Model: "claude-opus-5",
		}

		models, warnings := cfg.ResolveInlineModels()

		require.Empty(t, warnings)
		require.Len(t, models, 1)
		require.Equal(t, "claude-opus-5", models[0].Model)
	})

	t.Run("disabled providers are not matched", func(t *testing.T) {
		t.Parallel()
		cfg := inlineTestConfig()
		pc, _ := cfg.Providers.Get(claudecode.ProviderID)
		pc.Disable = true
		cfg.Providers.Set(claudecode.ProviderID, pc)
		cfg.Options.AgentModels = &AgentModelsOptions{
			Inline: []InlineModelRef{{Source: claudecode.ProviderID, Match: "*opus*"}},
		}

		models, warnings := cfg.ResolveInlineModels()

		require.Empty(t, models)
		require.Len(t, warnings, 1)
	})
}
