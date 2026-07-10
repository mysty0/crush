package codex

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/browser"

	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/callback"
)

// httpTimeout bounds every token/device HTTP call in this package.
const httpTimeout = 30 * time.Second

// LoginBrowser runs the full PKCE loopback authorization-code flow. It
// starts a callback server on the fixed port 1455 (no fallback), opens
// the authorization URL in the browser (also printing it), waits for the
// redirect, verifies the returned state, and exchanges the code for a
// token. The returned token's ExpiresAt is set from expires_in, and its
// access-token JWT embeds the ChatGPT account id for later use.
func LoginBrowser(ctx context.Context) (*oauth.Token, error) {
	pkce, err := oauth.GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("generate pkce: %w", err)
	}
	state, err := randomState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	srv, err := callback.Start(callback.Config{
		Port:              callbackPort,
		AllowPortFallback: false,
		Path:              callbackPath,
		Hostname:          "localhost",
	})
	if err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	defer srv.Close()

	redirectURI := srv.RedirectURI()
	authURL := authorizationURL(redirectURI, pkce.Challenge, state)

	fmt.Println("Opening your browser to authorize with OpenAI Codex.")
	fmt.Println("If it does not open automatically, visit:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	if err := browser.OpenURL(authURL); err != nil {
		slog.Debug("Could not open browser automatically", "error", err)
	}

	res, err := srv.Wait(ctx)
	if err != nil {
		return nil, err
	}
	if res.State != state {
		return nil, fmt.Errorf("state mismatch: possible CSRF attempt")
	}

	slog.Info("Exchanging authorization code for tokens")
	return Exchange(ctx, res.Code, pkce.Verifier, redirectURI)
}

// authorizationURL builds the browser authorization URL for the loopback
// flow.
func authorizationURL(redirectURI, challenge, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", originator)
	return authorizeURL + "?" + q.Encode()
}

// randomState returns a URL-safe random state value for CSRF protection.
func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// deviceUsercodeResponse is the response from the device usercode
// endpoint.
type deviceUsercodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	Interval     int    `json:"interval"`
}

// deviceTokenResponse is the response from a successful device token
// poll. The server supplies the code_verifier for the subsequent token
// exchange.
type deviceTokenResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

// LoginDevice runs the device-code flow for headless environments. It
// requests a user code, prints the verification URL and code to stdout,
// polls until the user authorizes, then exchanges the returned
// authorization code for a token using the device redirect URI.
func LoginDevice(ctx context.Context) (*oauth.Token, error) {
	uc, err := requestDeviceUsercode(ctx)
	if err != nil {
		return nil, err
	}

	fmt.Println("To authorize with OpenAI Codex, visit:")
	fmt.Println()
	fmt.Println("  " + deviceAuthURL)
	fmt.Println()
	fmt.Println("and enter the code:")
	fmt.Println()
	fmt.Println("  " + uc.UserCode)
	fmt.Println()
	fmt.Println("Waiting for authorization...")

	interval := time.Duration(uc.Interval) * time.Second
	if interval <= 0 {
		interval = devicePollIntervalSeconds * time.Second
	}

	for i := 0; i < deviceMaxPolls; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		tok, pending, err := pollDeviceToken(ctx, uc.DeviceAuthID, uc.UserCode)
		if err != nil {
			return nil, err
		}
		if pending {
			continue
		}

		slog.Info("Exchanging authorization code for tokens")
		return Exchange(ctx, tok.AuthorizationCode, tok.CodeVerifier, deviceRedirectURI)
	}

	return nil, fmt.Errorf("device authorization timed out")
}

// requestDeviceUsercode begins the device flow by requesting a user
// code.
func requestDeviceUsercode(ctx context.Context) (*deviceUsercodeResponse, error) {
	body, _ := json.Marshal(map[string]string{"client_id": clientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceUsercodeURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device usercode request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("device usercode request failed: %s - %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var uc deviceUsercodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&uc); err != nil {
		return nil, fmt.Errorf("decode device usercode: %w", err)
	}
	return &uc, nil
}

// pollDeviceToken polls the device token endpoint once. HTTP 403 and 404
// indicate the authorization is still pending (pending=true). A 200
// returns the authorization code and verifier.
func pollDeviceToken(ctx context.Context, deviceAuthID, userCode string) (*deviceTokenResponse, bool, error) {
	body, _ := json.Marshal(map[string]string{
		"device_auth_id": deviceAuthID,
		"user_code":      userCode,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("device token poll: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var tok deviceTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
			return nil, false, fmt.Errorf("decode device token: %w", err)
		}
		return &tok, false, nil
	case http.StatusForbidden, http.StatusNotFound:
		return nil, true, nil
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, false, fmt.Errorf("device token poll failed: %s - %s", resp.Status, strings.TrimSpace(string(b)))
	}
}

// Exchange trades an authorization code for an OAuth token using the
// authorization_code grant. The redirectURI must exactly match the one
// used to obtain the code (the loopback URI for the browser flow, or the
// device redirect URI for the device flow). The returned token's
// ExpiresAt is set from expires_in.
func Exchange(ctx context.Context, code, verifier, redirectURI string) (*oauth.Token, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", clientID)
	data.Set("code", code)
	data.Set("code_verifier", verifier)
	data.Set("redirect_uri", redirectURI)

	token, err := postToken(ctx, data)
	if err != nil {
		return nil, err
	}
	if DecodeAccountID(token.AccessToken) == "" {
		return nil, fmt.Errorf("access token missing chatgpt account id")
	}
	return token, nil
}

// postToken performs a form-encoded POST to the token endpoint and
// decodes the resulting OAuth token, setting ExpiresAt from expires_in.
func postToken(ctx context.Context, data url.Values) (*oauth.Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request failed: %s - %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var token oauth.Token
	if err := json.Unmarshal(b, &token); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	token.SetExpiresAt()
	return &token, nil
}
