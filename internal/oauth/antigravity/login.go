package antigravity

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

// devicePollIntervalSeconds is the fallback poll cadence for the device
// flow when the server does not supply one.
const devicePollIntervalSeconds = 5

// LoginBrowser runs a standard RFC 8252 loopback authorization-code flow
// against Google's OAuth server using Antigravity's public client
// registration.
//
// UNVERIFIED: the real Antigravity CLI appears to redirect through a
// fixed hosted page (https://antigravity.google/oauth-callback) rather
// than a bare loopback URI, which suggests its OAuth client may not
// accept arbitrary loopback ports. If Google rejects the redirect_uri
// here with "redirect_uri_mismatch", use LoginDevice instead. See
// docs/antigravity-cli-oauth-findings.md.
func LoginBrowser(ctx context.Context) (*oauth.Token, error) {
	state, err := randomState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	srv, err := callback.Start(callback.Config{
		Port:              callbackPort,
		AllowPortFallback: true,
		Path:              callbackPath,
		Hostname:          callbackHostname,
	})
	if err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	defer srv.Close()

	redirectURI := srv.RedirectURI()
	authorizeURL := buildAuthURL(redirectURI, state)

	fmt.Println("Opening your browser to authorize with Google Antigravity.")
	fmt.Println("If it does not open automatically, visit:")
	fmt.Println()
	fmt.Println("  " + authorizeURL)
	fmt.Println()
	if err := browser.OpenURL(authorizeURL); err != nil {
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
	return exchangeCode(ctx, res.Code, redirectURI)
}

// buildAuthURL constructs the Google authorization URL for the loopback
// flow.
func buildAuthURL(redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	return authURL + "?" + q.Encode()
}

// exchangeCode swaps an authorization code for tokens. A refresh token is
// required; its absence is a hard error because the token cannot be
// renewed without it.
func exchangeCode(ctx context.Context, code, redirectURI string) (*oauth.Token, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("code", code)
	data.Set("grant_type", "authorization_code")
	data.Set("redirect_uri", redirectURI)

	tok, err := postToken(ctx, data)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("antigravity: token exchange returned no refresh token")
	}
	return tok, nil
}

// deviceCodeResponse is the response from Google's device authorization
// endpoint.
type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// LoginDevice runs Google's standard RFC 8628 device-code flow for
// headless environments. It requests a user code, prints the verification
// URL and code to stdout, polls until the user authorizes, then returns
// the resulting token. Unlike LoginBrowser this flow does not depend on
// any redirect_uri registration, making it the more likely of the two to
// work unmodified with Antigravity's client (see the findings doc).
func LoginDevice(ctx context.Context) (*oauth.Token, error) {
	dc, err := requestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}

	fmt.Println("To authorize with Google Antigravity, visit:")
	fmt.Println()
	fmt.Println("  " + dc.VerificationURL)
	fmt.Println()
	fmt.Println("and enter the code:")
	fmt.Println()
	fmt.Println("  " + dc.UserCode)
	fmt.Println()
	fmt.Println("Waiting for authorization...")

	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = devicePollIntervalSeconds * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		tok, pending, err := pollDeviceToken(ctx, dc.DeviceCode)
		if err != nil {
			return nil, err
		}
		if pending {
			continue
		}
		return tok, nil
	}

	return nil, fmt.Errorf("device authorization timed out")
}

// requestDeviceCode begins the device flow by requesting a user code.
func requestDeviceCode(ctx context.Context) (*deviceCodeResponse, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("scope", strings.Join(scopes, " "))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("antigravity: device code request: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("antigravity: device code request failed: %s - %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var dc deviceCodeResponse
	if err := json.Unmarshal(b, &dc); err != nil {
		return nil, fmt.Errorf("antigravity: decode device code response: %w", err)
	}
	return &dc, nil
}

// pollDeviceToken polls the token endpoint once for the device flow.
// "authorization_pending" and "slow_down" indicate the authorization is
// still pending (pending=true); any other error is fatal.
func pollDeviceToken(ctx context.Context, deviceCode string) (*oauth.Token, bool, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("device_code", deviceCode)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("antigravity: device token poll: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, err
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &errResp)
		switch errResp.Error {
		case "authorization_pending", "slow_down":
			return nil, true, nil
		default:
			return nil, false, fmt.Errorf("antigravity: device token poll failed: %s - %s", resp.Status, strings.TrimSpace(string(b)))
		}
	}

	var tok oauth.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, false, fmt.Errorf("antigravity: decode token response: %w", err)
	}
	tok.SetExpiresAt()
	return &tok, false, nil
}

// randomState returns a URL-safe random anti-CSRF state value.
func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// postToken issues a form-urlencoded POST to the Google token endpoint and
// decodes the response, returning an error on non-2xx.
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
		return nil, fmt.Errorf("antigravity: token request: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("antigravity: token request failed: %s - %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var tok oauth.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, fmt.Errorf("antigravity: decode token response: %w", err)
	}
	tok.SetExpiresAt()
	return &tok, nil
}
