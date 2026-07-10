package codex

import (
	"context"
	"fmt"
	"net/url"

	"github.com/charmbracelet/crush/internal/oauth"
)

// RefreshToken exchanges a refresh token for a fresh access token using
// the refresh_token grant. When the provider returns an empty
// refresh_token, the caller's existing refresh token is preserved on the
// returned token. The returned token's ExpiresAt is set from expires_in,
// and its access-token JWT is validated to contain a ChatGPT account id.
func RefreshToken(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("codex: no refresh token available")
	}

	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("client_id", clientID)

	token, err := postToken(ctx, data)
	if err != nil {
		return nil, err
	}

	// A refresh may omit the refresh token; keep the existing one so the
	// caller can continue refreshing.
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	if DecodeAccountID(token.AccessToken) == "" {
		return nil, fmt.Errorf("codex: refreshed access token missing chatgpt account id")
	}
	return token, nil
}
