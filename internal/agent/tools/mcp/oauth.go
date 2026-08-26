package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth"
)

// oauthHTTPTimeout bounds the discovery and token HTTP calls.
const oauthHTTPTimeout = 30 * time.Second

// wellKnownPRM is the RFC 9728 well-known path for protected-resource
// metadata.
const wellKnownPRM = "/.well-known/oauth-protected-resource"

// errLoginRequired is returned when a server demands authorization but
// no usable token is stored. It carries the command the user needs to
// run, since the browser flow deliberately does not start on its own
// in the middle of an agent turn.
type errLoginRequired struct {
	name string
}

func (e *errLoginRequired) Error() string {
	return fmt.Sprintf("mcp %q requires authorization: run 'crush mcp login %s'", e.name, e.name)
}

// oauthHandler implements [auth.OAuthHandler] for MCP servers configured
// with OAuth 2.0.
//
// It deliberately does not use the SDK's auth.AuthorizationCodeHandler.
// That handler always requests every scope the server advertises, with
// no way to narrow them, and keeps its token source private with no hook
// to seed a stored token or observe refreshes. Crush needs both: scopes
// must be narrowable to what the user actually consented to, and tokens
// must survive a restart.
type oauthHandler struct {
	name      string
	store     *config.ConfigStore
	serverURL string
	client    *http.Client

	mu sync.Mutex
	ts oauth2.TokenSource
}

var _ auth.OAuthHandler = (*oauthHandler)(nil)

func newOAuthHandler(name, serverURL string, store *config.ConfigStore) *oauthHandler {
	return &oauthHandler{
		name:      name,
		store:     store,
		serverURL: serverURL,
		client:    &http.Client{Timeout: oauthHTTPTimeout},
	}
}

// TokenSource returns a token source backed by the stored token,
// refreshing it through the recorded token endpoint as needed. It
// returns a nil source when no token is stored, which leaves the
// request unauthenticated so the server can issue the challenge that
// drives Authorize.
func (h *oauthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ts != nil {
		return h.ts, nil
	}

	// Read config fresh: a "crush mcp login" in another process may
	// have stored a token since this session started.
	oc := oauthConfigFor(h.store, h.name)
	if oc == nil || oc.Token == nil || oc.Token.AccessToken == "" {
		return nil, nil
	}
	if oc.TokenURL == "" {
		// Nothing to refresh against. Usable only until it expires.
		if oc.Token.IsExpired() {
			return nil, nil
		}
		return oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: oc.Token.AccessToken,
			TokenType:   "Bearer",
		}), nil
	}

	secret, err := h.resolveSecret(*oc)
	if err != nil {
		return nil, err
	}

	cfg := &oauth2.Config{
		ClientID:     oc.ClientID,
		ClientSecret: secret,
		Endpoint:     oauth2.Endpoint{TokenURL: oc.TokenURL},
	}
	tok := &oauth2.Token{
		AccessToken:  oc.Token.AccessToken,
		RefreshToken: oc.Token.RefreshToken,
		TokenType:    "Bearer",
	}
	if oc.Token.ExpiresAt > 0 {
		tok.Expiry = time.Unix(oc.Token.ExpiresAt, 0)
	}

	name := h.name
	store := h.store
	h.ts = &persistingTokenSource{
		inner: cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, h.client), tok),
		last:  tok.AccessToken,
		save: func(t *oauth2.Token) {
			if err := storeToken(store, name, t); err != nil {
				slog.Error("Failed to persist refreshed MCP OAuth token", "name", name, "error", err)
			}
		},
	}
	return h.ts, nil
}

// Authorize reports that interactive login is required. Crush does not
// open a browser here: Authorize runs deep inside a tool call, where a
// surprise browser window and a blocked agent turn are both worse than
// a clear instruction. "crush mcp login" performs the real flow.
func (h *oauthHandler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	// Drop any cached source so the next attempt re-reads the config.
	h.mu.Lock()
	h.ts = nil
	h.mu.Unlock()
	return &errLoginRequired{name: h.name}
}

func (h *oauthHandler) resolveSecret(oc config.MCPOAuthConfig) (string, error) {
	if oc.ClientSecret == "" {
		return "", nil
	}
	secret, err := h.store.Resolver().ResolveValue(oc.ClientSecret)
	if err != nil {
		return "", fmt.Errorf("resolve oauth client secret for mcp %q: %w", h.name, err)
	}
	return strings.TrimSpace(secret), nil
}

// persistingTokenSource writes the token back to the config whenever the
// underlying source mints a new one.
type persistingTokenSource struct {
	inner oauth2.TokenSource
	save  func(*oauth2.Token)

	mu   sync.Mutex
	last string
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	changed := tok.AccessToken != p.last
	if changed {
		p.last = tok.AccessToken
	}
	p.mu.Unlock()
	if changed {
		p.save(tok)
	}
	return tok, nil
}

// oauthConfigFor returns the OAuth config for the named MCP server, or
// nil when the server is not configured for OAuth.
func oauthConfigFor(store *config.ConfigStore, name string) *config.MCPOAuthConfig {
	m, ok := store.Config().MCP[name]
	if !ok || m.OAuth == nil {
		return nil
	}
	return m.OAuth
}

// storeToken persists a token for the named MCP server in the global
// config scope.
func storeToken(store *config.ConfigStore, name string, tok *oauth2.Token) error {
	t := &oauth.Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
	}
	if !tok.Expiry.IsZero() {
		t.ExpiresAt = tok.Expiry.Unix()
		t.SetExpiresIn()
	}
	// A refresh response may omit the refresh token, which means "keep
	// using the one you have". Do not overwrite it with an empty value.
	if t.RefreshToken == "" {
		if oc := oauthConfigFor(store, name); oc != nil && oc.Token != nil {
			t.RefreshToken = oc.Token.RefreshToken
		}
	}
	return store.SetConfigField(config.ScopeGlobal, "mcp."+name+".oauth.token", t)
}

// discovered holds the endpoints and scopes resolved for a server.
type discovered struct {
	authorizationEndpoint string
	tokenEndpoint         string
	resource              string
	scopes                []string
}

// discover resolves the authorization server and scopes for an MCP
// server, following RFC 9728 protected-resource metadata and then RFC
// 8414 authorization-server metadata.
func discover(ctx context.Context, client *http.Client, serverURL string, oc config.MCPOAuthConfig) (*discovered, error) {
	prm, err := fetchPRM(ctx, client, serverURL)
	if err != nil && oc.AuthServerMetadataURL == "" {
		return nil, err
	}

	var asm *oauthex.AuthServerMeta
	switch {
	case oc.AuthServerMetadataURL != "":
		asm, err = oauthex.GetAuthServerMeta(ctx, oc.AuthServerMetadataURL, "", client)
		if err != nil {
			return nil, fmt.Errorf("fetch authorization server metadata: %w", err)
		}
	default:
		if len(prm.AuthorizationServers) == 0 {
			return nil, errors.New("server's protected-resource metadata lists no authorization servers")
		}
		asm, err = authServerMeta(ctx, client, prm.AuthorizationServers[0])
		if err != nil {
			return nil, fmt.Errorf("fetch authorization server metadata: %w", err)
		}
	}
	if asm.AuthorizationEndpoint == "" || asm.TokenEndpoint == "" {
		return nil, errors.New("authorization server metadata is missing required endpoints")
	}

	d := &discovered{
		authorizationEndpoint: asm.AuthorizationEndpoint,
		tokenEndpoint:         asm.TokenEndpoint,
		resource:              serverURL,
		scopes:                oc.Scopes,
	}
	if prm != nil {
		if prm.Resource != "" {
			d.resource = prm.Resource
		}
		if len(d.scopes) == 0 {
			d.scopes = prm.ScopesSupported
		}
	}
	if len(d.scopes) == 0 {
		return nil, errors.New("no scopes configured and the server advertises none; set mcp.<name>.oauth.scopes")
	}
	return d, nil
}

// authServerMeta fetches authorization server metadata for an issuer.
//
// RFC 8414 requires the issuer inside the metadata document to match
// the issuer used to look it up, exactly. Google violates this: the
// Gmail MCP server's protected-resource metadata names
// "https://accounts.google.com/" while the metadata document itself
// says "https://accounts.google.com". The well-known URL is the same
// either way, so retry without the trailing slash before giving up.
func authServerMeta(ctx context.Context, client *http.Client, issuer string) (*oauthex.AuthServerMeta, error) {
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, client)
	if err != nil {
		trimmed := strings.TrimSuffix(issuer, "/")
		if trimmed == issuer {
			return nil, err
		}
		asm, err = auth.GetAuthServerMetadata(ctx, trimmed, client)
		if err != nil {
			return nil, err
		}
		slog.Debug("Authorization server issuer matched only without its trailing slash", "issuer", issuer)
	}
	if asm == nil {
		return nil, fmt.Errorf("authorization server %q publishes no metadata", issuer)
	}
	return asm, nil
}

// fetchPRM retrieves the server's protected-resource metadata. It first
// asks the server directly, honouring the resource_metadata pointer in
// its WWW-Authenticate challenge, then falls back to the well-known
// locations described by RFC 9728.
func fetchPRM(ctx context.Context, client *http.Client, serverURL string) (*oauthex.ProtectedResourceMetadata, error) {
	var errs []error
	for _, candidate := range prmURLs(ctx, client, serverURL) {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, candidate, serverURL, client)
		if err == nil {
			return prm, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", candidate, err))
	}
	return nil, fmt.Errorf("could not fetch protected-resource metadata: %w", errors.Join(errs...))
}

// prmURLs returns candidate metadata URLs, most authoritative first.
func prmURLs(ctx context.Context, client *http.Client, serverURL string) []string {
	var candidates []string
	if u := challengeMetadataURL(ctx, client, serverURL); u != "" {
		candidates = append(candidates, u)
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return candidates
	}
	// RFC 9728 inserts the well-known segment before the resource path.
	if p := strings.Trim(u.Path, "/"); p != "" {
		withPath := *u
		withPath.Path = wellKnownPRM + "/" + p
		withPath.RawQuery = ""
		candidates = append(candidates, withPath.String())
	}
	root := *u
	root.Path = wellKnownPRM
	root.RawQuery = ""
	return append(candidates, root.String())
}

// challengeMetadataURL probes the MCP server unauthenticated and returns
// the resource_metadata URL from its WWW-Authenticate challenge, if any.
func challengeMetadataURL(ctx context.Context, client *http.Client, serverURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header[http.CanonicalHeaderKey("WWW-Authenticate")])
	if err != nil {
		return ""
	}
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			return u
		}
	}
	return ""
}
