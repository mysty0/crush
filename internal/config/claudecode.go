package config

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/charmbracelet/crush/internal/claudecode"
)

// storeTokenSource draws a Claude Code subscription token from a provider
// entry in the config store, refreshing (and persisting) it through the
// store's shared single-flighted refresh path so concurrent sessions and
// processes never race on the same rotation.
type storeTokenSource struct {
	store      *ConfigStore
	providerID string
}

// Token implements claudecode.TokenProvider.
func (t storeTokenSource) Token(ctx context.Context) (string, error) {
	pc, ok := t.store.Config().Providers.Get(t.providerID)
	if !ok || pc.OAuthToken == nil {
		return "", fmt.Errorf("claude code account %q is not logged in", t.providerID)
	}
	if !pc.OAuthToken.IsExpired() {
		return pc.OAuthToken.AccessToken, nil
	}

	stale := pc.OAuthToken.AccessToken
	if err := t.store.RefreshOAuthToken(ctx, ScopeGlobal, t.providerID); err != nil {
		// A stale token beats no token: the API is the authority on
		// whether it still works, and a refresh can fail for reasons
		// (offline, transient 5xx) that leave the token usable.
		if stale != "" {
			slog.Warn("Failed to refresh Claude Code token; using existing token",
				"provider", t.providerID, "error", err)
			return stale, nil
		}
		return "", err
	}

	refreshed, ok := t.store.Config().Providers.Get(t.providerID)
	if !ok || refreshed.OAuthToken == nil {
		return "", fmt.Errorf("claude code account %q lost its token during refresh", t.providerID)
	}
	return refreshed.OAuthToken.AccessToken, nil
}

// ClaudeCodeSource returns the token source for a Claude Code
// subscription provider. Accounts added with `crush login claude-code`
// are backed by the OAuth token stored in Crush's own config; the default
// "claude-code" provider, which predates that flow, falls back to the
// credentials file written by the official Claude Code CLI.
func (s *ConfigStore) ClaudeCodeSource(providerID string) *claudecode.Source {
	if pc, ok := s.Config().Providers.Get(providerID); ok && pc.OAuthToken != nil {
		return claudecode.NewTokenSource(storeTokenSource{store: s, providerID: providerID})
	}
	return claudecode.DefaultSource()
}

// ClaudeCodeAccounts returns the ids of every configured, enabled Claude
// Code subscription provider, in the config's provider order.
//
// A named account is included once it holds a token, since that is the
// only way it can authenticate. The default "claude-code" provider is
// included whenever it is configured: it authenticates from the Claude
// Code CLI's credentials file, and a missing file is better surfaced as a
// failure on use than by quietly dropping the account.
func (c *Config) ClaudeCodeAccounts() []string {
	var ids []string
	for id, pc := range c.Providers.Seq2() {
		if !claudecode.IsProviderID(id) || pc.Disable {
			continue
		}
		if pc.OAuthToken == nil && id != claudecode.ProviderID {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}
