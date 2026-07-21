package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
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
