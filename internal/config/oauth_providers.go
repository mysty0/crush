package config

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"charm.land/catwalk/pkg/catwalk"
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
func (c *Config) seedOAuthProviders(ctx context.Context) {
	disableDiscovery := false

	if pc, ok := c.Providers.Get(codex.ProviderID); ok && !pc.Disable && pc.OAuthToken != nil {
		pc.ID = codex.ProviderID
		pc.Name = cmp.Or(pc.Name, "OpenAI Codex")
		pc.Type = catwalk.TypeOpenAI
		pc.BaseURL = codex.BaseURL
		if len(pc.Models) == 0 {
			// Query the native /codex/models endpoint for the account's
			// actual model line-up, falling back to a static list.
			mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			pc.Models = codex.CachedModels(mctx, pc.OAuthToken.AccessToken)
			cancel()
		}
		pc.AutoDiscoverModels = &disableDiscovery
		c.Providers.Set(codex.ProviderID, pc)
	}

	if pc, ok := c.Providers.Get(geminicli.ProviderID); ok && !pc.Disable && pc.OAuthToken != nil {
		pc.ID = geminicli.ProviderID
		pc.Name = cmp.Or(pc.Name, "Gemini CLI")
		pc.Type = catwalk.TypeGoogle
		pc.BaseURL = geminicli.BaseURL
		if len(pc.Models) == 0 {
			// Query the native fetchAvailableModels endpoint for the
			// account's actual model line-up, falling back to a static
			// list.
			projectID := ""
			if pc.OAuthExtra != nil {
				projectID = pc.OAuthExtra["project_id"]
			}
			mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			models, err := geminicli.CachedModels(mctx, pc.OAuthToken.AccessToken, projectID, geminicli.GeminiCLIIdentity)
			cancel()
			pc.Models = models
			if err != nil {
				c.OAuthModelWarnings = append(c.OAuthModelWarnings, fmt.Sprintf(
					"%s: using a limited default model list (live model discovery failed: %s)", pc.Name, err,
				))
			}
		}
		pc.AutoDiscoverModels = &disableDiscovery
		c.Providers.Set(geminicli.ProviderID, pc)
	}

	// EXPERIMENTAL: see docs/antigravity-cli-oauth-findings.md. Reuses
	// geminicli's BaseURL and model discovery since Antigravity shares
	// that exact Cloud Code Assist backend and wire format.
	if pc, ok := c.Providers.Get(antigravity.ProviderID); ok && !pc.Disable && pc.OAuthToken != nil {
		pc.ID = antigravity.ProviderID
		pc.Name = cmp.Or(pc.Name, "Google Antigravity")
		pc.Type = catwalk.TypeGoogle
		pc.BaseURL = geminicli.BaseURL
		if len(pc.Models) == 0 {
			projectID := ""
			if pc.OAuthExtra != nil {
				projectID = pc.OAuthExtra["project_id"]
			}
			mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			models, err := geminicli.CachedModels(mctx, pc.OAuthToken.AccessToken, projectID, antigravity.Identity)
			cancel()
			pc.Models = models
			if err != nil {
				c.OAuthModelWarnings = append(c.OAuthModelWarnings, fmt.Sprintf(
					"%s: using a limited default model list (live model discovery failed: %s)", pc.Name, err,
				))
			}
		}
		pc.AutoDiscoverModels = &disableDiscovery
		c.Providers.Set(antigravity.ProviderID, pc)
	}
}
