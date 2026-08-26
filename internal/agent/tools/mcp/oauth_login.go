package mcp

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/pkg/browser"
	"golang.org/x/oauth2"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth/callback"
)

// readRedirectedURL prompts for the URL the browser was redirected to
// and extracts the authorization code and state from its query. It is
// the fallback for machines with no reachable browser, such as a remote
// container where the loopback callback cannot be delivered.
func readRedirectedURL(r io.Reader) (callback.Result, error) {
	fmt.Print("Redirected URL: ")
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return callback.Result{}, fmt.Errorf("read redirected URL: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return callback.Result{}, fmt.Errorf("no URL provided")
	}
	u, err := url.Parse(line)
	if err != nil {
		return callback.Result{}, fmt.Errorf("parse redirected URL: %w", err)
	}
	q := u.Query()
	return callback.Result{
		Code:             q.Get("code"),
		State:            q.Get("state"),
		Error:            q.Get("error"),
		ErrorDescription: q.Get("error_description"),
	}, nil
}

// Login runs the OAuth authorization-code flow for an MCP server and
// stores the resulting token in the global config.
func Login(ctx context.Context, store *config.ConfigStore, name string, noBrowser bool) error {
	m, ok := store.Config().MCP[name]
	if !ok {
		return fmt.Errorf("mcp %q not found in configuration", name)
	}
	if m.Type != config.MCPHttp && m.Type != config.MCPSSE {
		return fmt.Errorf("mcp %q uses the %s transport; OAuth applies to http and sse servers only", name, m.Type)
	}
	if m.OAuth == nil || m.OAuth.ClientID == "" {
		return fmt.Errorf("mcp %q has no oauth.client_id configured", name)
	}
	oc := *m.OAuth

	serverURL, err := m.ResolvedURL(store.Resolver())
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: oauthHTTPTimeout}
	d, err := discover(ctx, client, serverURL, oc)
	if err != nil {
		return err
	}

	handler := newOAuthHandler(name, serverURL, store)
	secret, err := handler.resolveSecret(oc)
	if err != nil {
		return err
	}

	// The redirect URI must match one registered with the authorization
	// server, so the port is used verbatim with no fallback.
	redirectURI := fmt.Sprintf("http://localhost:%d%s", oc.CallbackPortOrDefault(), oc.CallbackPathOrDefault())
	var srv *callback.Server
	if !noBrowser {
		srv, err = callback.Start(callback.Config{
			Port:              oc.CallbackPortOrDefault(),
			AllowPortFallback: false,
			Path:              oc.CallbackPathOrDefault(),
			Hostname:          "localhost",
		})
		if err != nil {
			return fmt.Errorf("start callback server: %w", err)
		}
		defer srv.Close()
		redirectURI = srv.RedirectURI()
	}

	cfg := &oauth2.Config{
		ClientID:     oc.ClientID,
		ClientSecret: secret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  d.authorizationEndpoint,
			TokenURL: d.tokenEndpoint,
		},
		RedirectURL: redirectURI,
		Scopes:      d.scopes,
	}

	verifier := oauth2.GenerateVerifier()
	state := rand.Text()
	authURL := cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("resource", d.resource),
		// Google only returns a refresh token when both of these are
		// present, and silently omits it on repeat consent without
		// prompt=consent. Other servers ignore unrecognized params.
		oauth2.SetAuthURLParam("access_type", "offline"),
		oauth2.SetAuthURLParam("prompt", "consent"),
	)

	fmt.Printf("Authorizing MCP server %q with scopes:\n", name)
	for _, s := range d.scopes {
		fmt.Println("  " + s)
	}
	fmt.Println()
	if noBrowser {
		fmt.Println("Open this URL in a browser:")
		fmt.Println()
		fmt.Println("  " + authURL)
		fmt.Println()
		fmt.Printf("After authorizing you will land on %s, which will not load.\n", redirectURI)
		fmt.Println("That is expected. Copy the full URL from the address bar and paste it here.")
		fmt.Println()
	} else {
		fmt.Println("Opening your browser. If it does not open automatically, visit:")
		fmt.Println()
		fmt.Println("  " + authURL)
		fmt.Println()
		if err := browser.OpenURL(authURL); err != nil {
			slog.Debug("Could not open browser automatically", "error", err)
		}
	}

	var res callback.Result
	if noBrowser {
		res, err = readRedirectedURL(os.Stdin)
	} else {
		res, err = srv.Wait(ctx)
	}
	if err != nil {
		return err
	}
	if res.Error != "" {
		return fmt.Errorf("authorization failed: %s", cmp.Or(res.ErrorDescription, res.Error))
	}
	if res.State != state {
		return fmt.Errorf("state mismatch: possible CSRF attempt")
	}

	tok, err := cfg.Exchange(
		context.WithValue(ctx, oauth2.HTTPClient, client),
		res.Code,
		oauth2.VerifierOption(verifier),
		oauth2.SetAuthURLParam("resource", d.resource),
	)
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}
	if tok.RefreshToken == "" {
		slog.Warn("Authorization server returned no refresh token; re-authorization will be needed when the access token expires", "name", name)
	}

	// Record the token endpoint so refreshes skip discovery.
	if err := store.SetConfigField(config.ScopeGlobal, "mcp."+name+".oauth.token_url", d.tokenEndpoint); err != nil {
		return err
	}
	if err := storeToken(store, name, tok); err != nil {
		return err
	}

	fmt.Printf("Authorized. Token stored for mcp %q.\n", name)
	return nil
}

// Logout removes the stored OAuth token for an MCP server.
func Logout(store *config.ConfigStore, name string) error {
	if _, ok := store.Config().MCP[name]; !ok {
		return fmt.Errorf("mcp %q not found in configuration", name)
	}
	if err := store.RemoveConfigField(config.ScopeGlobal, "mcp."+name+".oauth.token"); err != nil {
		return err
	}
	fmt.Printf("Removed stored OAuth token for mcp %q.\n", name)
	return nil
}
