package claudecode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/browser"

	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/callback"
)

const (
	// authorizeURL is the claude.ai subscription authorization page. The
	// official client bounces sign-ins through claude.com/cai/* (which
	// 307s to claude.ai/oauth/authorize) for attribution; Crush uses the
	// same entry point so a subscription login behaves identically.
	authorizeURL = "https://claude.com/cai/oauth/authorize"

	// manualRedirectURI is the hosted redirect used by the paste-the-code
	// flow. Its success page displays "code#state" for the user to copy.
	manualRedirectURI = "https://platform.claude.com/oauth/code/callback"

	// callbackPath is the loopback path the authorization redirect hits.
	// The port is ephemeral: Anthropic accepts any localhost port for this
	// client, so nothing has to be reserved.
	callbackPath = "/callback"
)

// Scopes is the full set requested at login, matching the official
// client. Requesting all of them up front means one consent screen covers
// both the Console and Claude.ai halves of the flow.
var Scopes = append([]string{"org:create_api_key"}, subscriptionScopes...)

// subscriptionScopes is the Claude.ai (Pro/Max/Team/Enterprise) half of
// the scope list, and the set a refresh asks for. The Console
// api-key-minting scope is deliberately not replayed on refresh: the
// official client only requests it at authorization time.
var subscriptionScopes = []string{
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

// Account identifies the subscription a token belongs to. Anthropic
// returns it with the token itself, so Crush can label accounts (and
// detect the same account being added twice) without a profile request.
type Account struct {
	// UUID is the Anthropic account id.
	UUID string
	// Email is the account's email address, used as the display label.
	Email string
	// OrganizationUUID is the account's organization, if any.
	OrganizationUUID string
	// OrganizationName is the human-readable organization name, if any.
	OrganizationName string
}

// Label returns the best available human-readable name for the account,
// falling back to the organization name and then the account UUID.
func (a Account) Label() string {
	switch {
	case a.Email != "":
		return a.Email
	case a.OrganizationName != "":
		return a.OrganizationName
	default:
		return a.UUID
	}
}

// LoginBrowser runs the PKCE authorization-code flow against a loopback
// listener: it opens the consent page in a browser, waits for the
// redirect, verifies the state, and exchanges the code for a token.
func LoginBrowser(ctx context.Context) (*oauth.Token, Account, error) {
	pkce, err := oauth.GeneratePKCE()
	if err != nil {
		return nil, Account{}, fmt.Errorf("claudecode: generate pkce: %w", err)
	}
	state, err := randomState()
	if err != nil {
		return nil, Account{}, fmt.Errorf("claudecode: generate state: %w", err)
	}

	srv, err := callback.Start(callback.Config{
		Path:     callbackPath,
		Hostname: "localhost",
	})
	if err != nil {
		return nil, Account{}, fmt.Errorf("claudecode: start callback server: %w", err)
	}
	defer srv.Close()

	redirectURI := srv.RedirectURI()
	authURL := authorizationURL(redirectURI, pkce.Challenge, state)

	fmt.Println("Opening your browser to authorize with your Claude subscription.")
	fmt.Println("If it does not open automatically, visit:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	if err := browser.OpenURL(authURL); err != nil {
		slog.Debug("Could not open browser automatically", "error", err)
	}

	res, err := srv.Wait(ctx)
	if err != nil {
		return nil, Account{}, err
	}
	// The redirect echoes state; a mismatch means the response did not
	// originate from the request we started.
	if res.State != state {
		return nil, Account{}, fmt.Errorf("claudecode: state mismatch: possible CSRF attempt")
	}

	return Exchange(ctx, res.Code, res.State, pkce.Verifier, redirectURI)
}

// LoginManual runs the same flow without a loopback listener, for
// headless machines and remote shells: the user authorizes in a browser
// anywhere, then pastes back the "code#state" fragment the success page
// displays.
func LoginManual(ctx context.Context) (*oauth.Token, Account, error) {
	pkce, err := oauth.GeneratePKCE()
	if err != nil {
		return nil, Account{}, fmt.Errorf("claudecode: generate pkce: %w", err)
	}
	state, err := randomState()
	if err != nil {
		return nil, Account{}, fmt.Errorf("claudecode: generate state: %w", err)
	}

	authURL := authorizationURL(manualRedirectURI, pkce.Challenge, state)
	fmt.Println("Open the following URL in a browser and authorize with your Claude subscription:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Println("Then paste the code shown on the success page and press enter:")
	fmt.Print("> ")

	var pasted string
	if _, err := fmt.Scanln(&pasted); err != nil {
		return nil, Account{}, fmt.Errorf("claudecode: read pasted code: %w", err)
	}

	code, pastedState := splitManualCode(pasted)
	if code == "" {
		return nil, Account{}, fmt.Errorf("claudecode: no authorization code pasted")
	}
	// The success page appends the state to the code; when it is present
	// it must match, but a user who pasted only the code is not blocked.
	if pastedState != "" && pastedState != state {
		return nil, Account{}, fmt.Errorf("claudecode: state mismatch: possible CSRF attempt")
	}

	return Exchange(ctx, code, state, pkce.Verifier, manualRedirectURI)
}

// splitManualCode splits the "code#state" value shown on the hosted
// success page. A value without a fragment is treated as a bare code.
func splitManualCode(pasted string) (code, state string) {
	pasted = strings.TrimSpace(pasted)
	code, state, _ = strings.Cut(pasted, "#")
	return strings.TrimSpace(code), strings.TrimSpace(state)
}

// authorizationURL builds the consent-page URL for either redirect style.
func authorizationURL(redirectURI, challenge, state string) string {
	q := url.Values{}
	// "code=true" is what makes the page offer the Claude Max/Pro
	// subscription sign-in rather than only Console API-key auth.
	q.Set("code", "true")
	q.Set("client_id", oauthClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(Scopes, " "))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return authorizeURL + "?" + q.Encode()
}

// Exchange trades an authorization code for a subscription token. The
// redirect URI must match the one the code was issued for.
func Exchange(ctx context.Context, code, state, verifier, redirectURI string) (*oauth.Token, Account, error) {
	body, _ := json.Marshal(map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
		"client_id":     oauthClientID,
		"code_verifier": verifier,
		"state":         state,
	})
	tr, err := postToken(ctx, loginClient(), body)
	if err != nil {
		return nil, Account{}, fmt.Errorf("claudecode: exchange authorization code: %w", err)
	}
	return tokenFrom(tr), accountFrom(tr), nil
}

// RefreshToken runs the refresh_token grant for a stored subscription
// account. It backs the config store's generic OAuth refresh path, so
// accounts added by `crush login claude-code` rotate like any other
// OAuth provider.
func RefreshToken(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	tr, err := exchangeRefreshToken(ctx, loginClient(), refreshToken, "")
	if err != nil {
		return nil, err
	}
	token := tokenFrom(tr)
	// Anthropic omits the refresh token when it has not rotated; keeping
	// the old one prevents a silent logout on the next refresh.
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	return token, nil
}

func tokenFrom(tr tokenResponse) *oauth.Token {
	token := &oauth.Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresIn:    int(tr.ExpiresIn),
	}
	token.SetExpiresAt()
	return token
}

func accountFrom(tr tokenResponse) Account {
	return Account{
		UUID:             tr.Account.UUID,
		Email:            tr.Account.EmailAddress,
		OrganizationUUID: tr.Organization.UUID,
		OrganizationName: tr.Organization.Name,
	}
}

func loginClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// randomState returns a URL-safe random state value for CSRF protection.
func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
