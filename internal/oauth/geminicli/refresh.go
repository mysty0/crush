package geminicli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/oauth"
)

// tokenResponse is the parsed body of a Google OAuth token endpoint
// response, for both authorization_code exchange and refresh_token.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// RefreshToken exchanges a refresh token for a fresh access token. The
// previously discovered projectID is required so it can be re-attached to
// the returned token (the Cloud Code Assist backend needs it on every
// inference request). If Google omits a new refresh token the old one is
// retained.
func RefreshToken(ctx context.Context, refreshToken, projectID string) (*oauth.Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("geminicli: no refresh token available")
	}

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret())
	data.Set("refresh_token", refreshToken)
	data.Set("grant_type", "refresh_token")

	resp, err := postToken(ctx, data)
	if err != nil {
		return nil, err
	}

	newRefresh := resp.RefreshToken
	if newRefresh == "" {
		newRefresh = refreshToken
	}

	tok := &oauth.Token{
		AccessToken:  resp.AccessToken,
		RefreshToken: newRefresh,
		ExpiresIn:    applySkew(resp.ExpiresIn),
	}
	tok.SetExpiresAt()
	return tok, nil
}

// applySkew subtracts the refresh skew from a reported lifetime, keeping it
// non-negative so SetExpiresAt still produces a sensible expiry.
func applySkew(expiresIn int) int {
	v := expiresIn - refreshSkewSeconds
	if v < 0 {
		return 0
	}
	return v
}

// postToken issues a form-urlencoded POST to the Google token endpoint and
// decodes the response, returning an error on non-2xx.
func postToken(ctx context.Context, data url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geminicli: token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geminicli: token request failed: %s - %s", resp.Status, string(body))
	}

	var out tokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("geminicli: parse token response: %w", err)
	}
	return &out, nil
}
