package geminicli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/callback"
	"github.com/pkg/browser"
)

// LoginBrowser runs the full Gemini CLI loopback authorization-code flow:
// it starts a local callback server, opens the Google consent page in the
// browser, exchanges the returned code for tokens, resolves the user's
// email, and discovers the Cloud Code Assist project. It returns the token
// (with access, refresh, and a skewed expiry populated), the discovered
// Cloud project id, and the account email (which may be empty).
//
// The Google installed-app flow uses the client secret rather than PKCE, so
// no code verifier is sent.
func LoginBrowser(ctx context.Context) (token *oauth.Token, projectID, email string, err error) {
	state, err := randomState()
	if err != nil {
		return nil, "", "", err
	}

	server, err := callback.Start(callback.Config{
		Port:              callbackPort,
		AllowPortFallback: true,
		Path:              callbackPath,
		Hostname:          callbackHostname,
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("geminicli: start callback server: %w", err)
	}
	defer server.Close()

	redirectURI := server.RedirectURI()
	authorizeURL := buildAuthURL(redirectURI, state)

	if err := browser.OpenURL(authorizeURL); err != nil {
		fmt.Printf("Open this URL in your browser to sign in:\n\n%s\n\n", authorizeURL)
	} else {
		fmt.Printf("Opening your browser to sign in. If it does not open, visit:\n\n%s\n\n", authorizeURL)
	}

	res, err := server.Wait(ctx)
	if err != nil {
		return nil, "", "", err
	}
	if res.State != state {
		return nil, "", "", fmt.Errorf("geminicli: state mismatch; possible CSRF")
	}

	tok, err := exchangeCode(ctx, res.Code, redirectURI)
	if err != nil {
		return nil, "", "", err
	}

	// The account email is best-effort; failures are ignored.
	email, _ = fetchEmail(ctx, tok.AccessToken)

	projectID, err = DiscoverProject(ctx, tok.AccessToken, GeminiCLIIdentity)
	if err != nil {
		return nil, "", "", err
	}

	return tok, projectID, email, nil
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
	data.Set("client_secret", clientSecret())
	data.Set("code", code)
	data.Set("grant_type", "authorization_code")
	data.Set("redirect_uri", redirectURI)

	resp, err := postToken(ctx, data)
	if err != nil {
		return nil, err
	}
	if resp.RefreshToken == "" {
		return nil, fmt.Errorf("geminicli: token exchange returned no refresh token")
	}

	tok := &oauth.Token{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresIn:    applySkew(resp.ExpiresIn),
	}
	tok.SetExpiresAt()
	return tok, nil
}

// fetchEmail retrieves the authenticated user's email address. Errors are
// returned so the caller can decide to ignore them.
func fetchEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("geminicli: userinfo failed: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var out struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.Email, nil
}

// randomState returns a URL-safe random anti-CSRF state value.
func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
