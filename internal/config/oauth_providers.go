package config

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/charmbracelet/crush/internal/oauth/codex"
	"github.com/charmbracelet/crush/internal/oauth/geminicli"
)

// seedOAuthProviders populates the runtime provider metadata (type, base
// URL, and model list) for the OAuth-only providers whose credentials were
// written by `crush login` but whose wire configuration is not persisted.
// It mirrors the native Claude Code provider: the login flow stores only
// the token, and the fixed endpoint/model shape is re-applied on every load
// so the provider is never dropped as an unconfigured custom provider.
//
// It runs before custom-provider validation and model discovery so the
// seeded providers are treated as fully configured and skip discovery.
func (c *Config) seedOAuthProviders(ctx context.Context, store *ConfigStore) {
	disableDiscovery := false

	c.seedClaudeCodeAccounts(ctx, store, &disableDiscovery)

	if pc, ok := c.Providers.Get(codex.ProviderID); ok && !pc.Disable && pc.OAuthToken != nil {
		pc.ID = codex.ProviderID
		pc.Name = cmp.Or(pc.Name, "OpenAI Codex")
		pc.Type = catwalk.TypeOpenAI
		pc.BaseURL = codex.BaseURL
		pc = c.refreshOAuthProviderBeforeModelDiscovery(ctx, store, codex.ProviderID, pc)
		if len(pc.Models) == 0 {
			// The static default list is used instantly unless this is
			// the account behind the user's current model selection, in
			// which case its live model list -- fetched from the native
			// /codex/models endpoint -- is needed now to validate that
			// selection; see seedModels.
			token := pc.OAuthToken.AccessToken
			pc.Models = seedModels(c, store, codex.ProviderID, pc.Name, codex.DefaultModels(),
				func(ctx context.Context) ([]catwalk.Model, error) {
					return codex.CachedModels(ctx, token)
				})
		}
		pc.AutoDiscoverModels = &disableDiscovery
		c.Providers.Set(codex.ProviderID, pc)
	}

	if pc, ok := c.Providers.Get(geminicli.ProviderID); ok && !pc.Disable && pc.OAuthToken != nil {
		pc.ID = geminicli.ProviderID
		pc.Name = cmp.Or(pc.Name, "Gemini CLI")
		pc.Type = catwalk.TypeGoogle
		pc.BaseURL = geminicli.BaseURL
		pc = c.refreshOAuthProviderBeforeModelDiscovery(ctx, store, geminicli.ProviderID, pc)
		if len(pc.Models) == 0 {
			// See the codex case above for why this is sometimes
			// synchronous.
			projectID := ""
			if pc.OAuthExtra != nil {
				projectID = pc.OAuthExtra["project_id"]
			}
			token := pc.OAuthToken.AccessToken
			pc.Models = seedModels(c, store, geminicli.ProviderID, pc.Name, geminicli.DefaultModels(),
				func(ctx context.Context) ([]catwalk.Model, error) {
					return geminicli.CachedModels(ctx, token, projectID, geminicli.GeminiCLIIdentity)
				})
		}
		pc.AutoDiscoverModels = &disableDiscovery
		c.Providers.Set(geminicli.ProviderID, pc)
	}

	// EXPERIMENTAL: see docs/antigravity-cli-oauth-findings.md. Reuses
	// geminicli's model discovery/wire format since Antigravity shares
	// that Cloud Code Assist wire format, but NOT its BaseURL: confirmed
	// by live traffic comparison that Antigravity's host resolves the
	// same OAuth token to a different (working) backend project -- see
	// antigravity.BaseURL's doc comment.
	if pc, ok := c.Providers.Get(antigravity.ProviderID); ok && !pc.Disable && pc.OAuthToken != nil {
		pc.ID = antigravity.ProviderID
		pc.Name = cmp.Or(pc.Name, "Google Antigravity")
		pc.Type = catwalk.TypeGoogle
		pc.BaseURL = antigravity.BaseURL
		pc = c.refreshOAuthProviderBeforeModelDiscovery(ctx, store, antigravity.ProviderID, pc)
		if len(pc.Models) == 0 {
			projectID := ""
			if pc.OAuthExtra != nil {
				projectID = pc.OAuthExtra["project_id"]
			}
			token := pc.OAuthToken.AccessToken
			pc.Models = seedModels(c, store, antigravity.ProviderID, pc.Name, geminicli.DefaultModels(),
				func(ctx context.Context) ([]catwalk.Model, error) {
					return geminicli.CachedModels(ctx, token, projectID, antigravity.Identity)
				})
		}
		pc.AutoDiscoverModels = &disableDiscovery
		c.Providers.Set(antigravity.ProviderID, pc)
	}
}

// seedModelsInBackground returns fallback immediately so Load never
// blocks on a live model-discovery call, and fetches the account's real
// model line-up in the background via fetch. Once the fetch succeeds,
// the result is merged into the live config store so a session started
// moments after Crush launches still ends up with the account's actual
// models, without holding up startup for it. A failed fetch is silently
// left as fallback: it is not a warning-worthy condition here since the
// synchronous behavior it replaces already used the same fallback on
// error.
func seedModelsInBackground(store *ConfigStore, providerID string, fallback []catwalk.Model, fetch func(ctx context.Context) ([]catwalk.Model, error)) []catwalk.Model {
	// Skipped under test: this would otherwise fire a real network call
	// with a goroutine that outlives its originating test.
	if testing.Testing() {
		return fallback
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := fetch(ctx)
		if err != nil || len(models) == 0 {
			return
		}
		store.mutateInMemory(func(nc *Config) {
			if pc, ok := nc.Providers.Get(providerID); ok {
				pc.Models = models
				nc.Providers.Set(providerID, pc)
			}
		})
		store.SetupAgents()
	}()
	return fallback
}

// isSelectedModelProvider reports whether providerID is the provider
// behind the user's currently configured large or small model. Compared
// against the raw configured selection (cfg.Models), not the resolved
// default -- this runs before resolveSelectedModels, and exists so that
// exact provider can be checked for validation, above.
func isSelectedModelProvider(cfg *Config, providerID string) bool {
	for _, mt := range []SelectedModelType{SelectedModelTypeLarge, SelectedModelTypeSmall} {
		if sel, ok := cfg.Models[mt]; ok && sel.Provider == providerID {
			return true
		}
	}
	return false
}

// seedModels resolves an OAuth-subscription provider's model list,
// either synchronously or in the background, depending on whether
// providerID is the provider behind the user's currently configured
// large or small model (see isSelectedModelProvider).
//
// A provider the user isn't currently pointed at can safely defer to
// the background (seedModelsInBackground): Load returns fast and the
// live list is spliced in moments later, well before anyone is likely
// to switch to it.
//
// But the provider the user IS currently pointed at must be resolved
// synchronously. Load validates the configured model against exactly
// the list returned here a few lines later (resolveSelectedModels):
// deferring this fetch would mean that check runs against only the
// static fallback list, which is missing any model newer than this
// release's fallback -- e.g. one already selected in a previous
// session, once the account gained access to it. Every affected
// launch would then falsely conclude the user's valid selection is
// "unavailable", substitute an unrelated fallback model, and surface a
// misleading warning -- even though the model was never actually
// unavailable, only not yet fetched.
func seedModels(cfg *Config, store *ConfigStore, providerID, name string, fallback []catwalk.Model, fetch func(ctx context.Context) ([]catwalk.Model, error)) []catwalk.Model {
	if !isSelectedModelProvider(cfg, providerID) {
		return seedModelsInBackground(store, providerID, fallback, fetch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	models, err := fetch(ctx)
	if err != nil || len(models) == 0 {
		if err != nil {
			cfg.OAuthModelWarnings = append(cfg.OAuthModelWarnings, fmt.Sprintf(
				"%s: using a limited default model list (live model discovery failed: %s)", name, err,
			))
		}
		return fallback
	}
	return models
}

// seedClaudeCodeAccounts populates the wire configuration for every
// Claude Code subscription account added with `crush login claude-code`.
// As with the other OAuth-only providers, the login flow persists only
// the token: the fixed Anthropic endpoint, provider type, and the
// account's own model list are re-applied on every load so an account is
// never dropped as an unconfigured custom provider.
//
// The default "claude-code" provider is skipped here when it has no
// stored token, since it authenticates from the Claude Code CLI's
// credentials file instead (see load.go).
func (c *Config) seedClaudeCodeAccounts(ctx context.Context, store *ConfigStore, disableDiscovery *bool) {
	for id, pc := range c.Providers.Seq2() {
		if !claudecode.IsProviderID(id) || pc.Disable || pc.OAuthToken == nil {
			continue
		}
		pc.ID = id
		pc.Name = cmp.Or(pc.Name, claudeCodeAccountName(id, pc))
		pc.Type = catwalk.TypeAnthropic
		pc.BaseURL = claudecode.BaseURL
		// Only the endpoint, type, and model list are owned here. Every
		// other field an account may carry (name, flat_rate, ...) is left
		// as configured, so a logged-in account can still be tuned from
		// crush.json exactly like the default one.
		pc = c.refreshOAuthProviderBeforeModelDiscovery(ctx, store, id, pc)

		if len(pc.Models) == 0 {
			// Two accounts can sit on different plans and so see
			// different model line-ups; each is queried and cached under
			// its own provider id. The token was just refreshed above,
			// so it is handed over directly — resolving it through the
			// store here would try to take the config write lock the
			// loader already holds. The default list is used instantly
			// unless this account is behind the user's current model
			// selection; see seedModels.
			token := pc.OAuthToken.AccessToken
			src := claudecode.NewTokenSource(claudecode.TokenFunc(
				func(context.Context) (string, error) { return token, nil },
			))
			pc.Models = seedModels(c, store, id, pc.Name, claudecode.DefaultModels(),
				func(ctx context.Context) ([]catwalk.Model, error) {
					return claudecode.CachedModelsFor(ctx, id, src), nil
				})
		}
		pc.AutoDiscoverModels = disableDiscovery
		c.Providers.Set(id, pc)
	}
}

// claudeCodeAccountName builds the display name shown in the model
// picker for a subscription account, preferring the account name from the
// provider id and falling back to the email the login flow recorded.
func claudeCodeAccountName(providerID string, pc ProviderConfig) string {
	if account := claudecode.AccountFromProviderID(providerID); account != "" {
		return fmt.Sprintf("Claude Code (%s)", account)
	}
	if pc.OAuthExtra != nil && pc.OAuthExtra["email"] != "" {
		return fmt.Sprintf("Claude Code (%s)", pc.OAuthExtra["email"])
	}
	return "Claude Code"
}

func (c *Config) refreshOAuthProviderBeforeModelDiscovery(ctx context.Context, store *ConfigStore, providerID string, pc ProviderConfig) ProviderConfig {
	if store == nil || pc.OAuthToken == nil || !pc.OAuthToken.IsExpired() {
		return pc
	}

	if err := store.refreshOAuthTokenNoReload(ctx, ScopeGlobal, providerID); err != nil {
		name := cmp.Or(pc.Name, providerID)
		c.OAuthModelWarnings = append(c.OAuthModelWarnings, fmt.Sprintf(
			"%s: OAuth token refresh failed before live model discovery: %s", name, err,
		))
		slog.Warn("Failed to refresh OAuth token before model discovery", "provider", providerID, "error", err)
		return pc
	}

	if refreshed, ok := c.Providers.Get(providerID); ok && refreshed.OAuthToken != nil {
		return refreshed
	}
	return pc
}
