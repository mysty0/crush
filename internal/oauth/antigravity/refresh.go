package antigravity

import (
	"context"
	"fmt"
	"net/url"

	"github.com/charmbracelet/crush/internal/oauth"
)

// RefreshToken exchanges a refresh token for a fresh access token using
// Antigravity's client registration. If Google omits a new refresh token
// the old one is retained.
func RefreshToken(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("antigravity: no refresh token available")
	}

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("refresh_token", refreshToken)
	data.Set("grant_type", "refresh_token")

	tok, err := postToken(ctx, data)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}
