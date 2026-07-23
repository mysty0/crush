// Package claudecode provides native, cross-platform support for driving a
// Claude Code (Claude Pro/Max) subscription from Crush. It reads and
// refreshes the OAuth token stored by the official Claude Code CLI in
// ~/.claude/.credentials.json, injects it on outgoing Anthropic requests,
// and queries the subscription's available models from /v1/models.
//
// This replaces the external shell / PowerShell helper scripts, so the
// integration behaves identically on Linux, macOS, and Windows with no
// runtime dependency on bash, jq, pwsh, or curl.
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

const (
	// ProviderID is the reserved Crush provider id that activates native
	// Claude Code subscription handling (auth + model discovery). It is
	// the default account; additional accounts live under
	// "claude-code-<account>" (see ProviderIDForAccount).
	ProviderID = "claude-code"

	// BaseURL is the Anthropic API base used for the subscription.
	BaseURL = "https://api.anthropic.com"

	// OAuthBeta is the anthropic-beta flag that authorizes a subscription
	// OAuth token for inference.
	OAuthBeta = "oauth-2025-04-20"

	oauthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	tokenURL         = "https://platform.claude.com/v1/oauth/token"
	anthropicVersion = "2023-06-01"
	userAgent        = "claude-cli/2.1.196 (external, cli)"

	// refreshSkewMS refreshes the token this long (5 min) before expiry.
	refreshSkewMS = 5 * 60 * 1000
)

// IsProviderID reports whether providerID addresses a Claude Code
// subscription account: either the default "claude-code" provider or one
// of the additional accounts stored as "claude-code-<account>".
func IsProviderID(providerID string) bool {
	return providerID == ProviderID || strings.HasPrefix(providerID, ProviderID+"-")
}

// ProviderIDForAccount returns the provider id holding the named
// subscription account. An empty (or "default") name maps to the base
// provider, so `crush login claude-code` without --account keeps using
// the same provider id it always has.
func ProviderIDForAccount(account string) string {
	slug := AccountSlug(account)
	if slug == "" || slug == "default" {
		return ProviderID
	}
	return ProviderID + "-" + slug
}

// AccountFromProviderID returns the account name encoded in a Claude Code
// provider id, or "" for the default provider.
func AccountFromProviderID(providerID string) string {
	if providerID == ProviderID || !IsProviderID(providerID) {
		return ""
	}
	return strings.TrimPrefix(providerID, ProviderID+"-")
}

// AccountSlug normalizes a user-supplied account name into the form used
// inside a provider id: lowercase, with every run of characters outside
// [a-z0-9] collapsed to a single dash and the ends trimmed. An account
// name that normalizes to nothing (e.g. "!!!") yields "".
func AccountSlug(account string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(account)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// CredentialsPath returns the path to the Claude Code credentials file,
// honoring the CLAUDE_CREDENTIALS override. Returns "" if the home
// directory cannot be determined.
func CredentialsPath() string {
	if p := os.Getenv("CLAUDE_CREDENTIALS"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// Available reports whether a Claude Code credentials file is present.
func Available() bool {
	p := CredentialsPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// TokenProvider supplies a valid access token for one subscription
// account. Accounts added with `crush login claude-code` keep their token
// in Crush's own config instead of the Claude Code credentials file; the
// config package supplies an implementation that refreshes and persists
// through the config store.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// TokenFunc adapts a plain function to TokenProvider, for callers that
// already hold a valid token and only need to hand it over.
type TokenFunc func(ctx context.Context) (string, error)

// Token implements TokenProvider.
func (f TokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// Source reads and refreshes Claude Code OAuth tokens. It is safe for
// concurrent use; refreshes are serialized so a single rotation is shared.
//
// A Source is backed either by the Claude Code CLI credentials file (the
// default account) or by a TokenProvider (every account Crush logged in
// itself). Everything downstream — model discovery, usage, the auth
// transport — works the same way against both.
type Source struct {
	mu      sync.Mutex
	path    string
	tokens  TokenProvider
	client  *http.Client
	baseURL string
}

var (
	defaultOnce   sync.Once
	defaultSource *Source
)

// DefaultSource returns a process-wide Source so token refreshes are
// serialized across the auth transport and model discovery.
func DefaultSource() *Source {
	defaultOnce.Do(func() { defaultSource = NewSource() })
	return defaultSource
}

// NewSource creates a Source bound to the current credentials path.
func NewSource() *Source {
	return &Source{
		path:   CredentialsPath(),
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// NewTokenSource creates a Source that draws its token from p rather than
// from the Claude Code credentials file.
func NewTokenSource(p TokenProvider) *Source {
	return &Source{
		tokens: p,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// tokenResponse is the /v1/oauth/token payload. Anthropic echoes the
// authenticated account and organization alongside the tokens, which is
// what lets Crush label each subscription account without a second
// profile round-trip.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Account      struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

// Token returns a valid access token, refreshing and persisting a new one
// when the stored token is missing or within the refresh skew of expiry.
// If a refresh fails but a (possibly stale) token exists, that token is
// returned rather than erroring — the API is the source of truth on
// validity.
func (s *Source) Token(ctx context.Context) (string, error) {
	if s.tokens != nil {
		return s.tokens.Token(ctx)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return "", fmt.Errorf("claudecode: cannot locate credentials file")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return "", fmt.Errorf("claudecode: read credentials: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", fmt.Errorf("claudecode: parse credentials: %w", err)
	}
	oauthRaw, ok := root["claudeAiOauth"]
	if !ok {
		return "", fmt.Errorf("claudecode: credentials missing claudeAiOauth")
	}
	var oauth map[string]json.RawMessage
	if err := json.Unmarshal(oauthRaw, &oauth); err != nil {
		return "", fmt.Errorf("claudecode: parse claudeAiOauth: %w", err)
	}

	access := jsonString(oauth["accessToken"])
	expiresAt := jsonInt(oauth["expiresAt"])
	nowMS := time.Now().UnixMilli()

	if access != "" && expiresAt-nowMS > refreshSkewMS {
		return access, nil
	}

	refresh := jsonString(oauth["refreshToken"])
	if refresh == "" {
		if access != "" {
			return access, nil
		}
		return "", fmt.Errorf("claudecode: no refresh token available")
	}

	tok, err := s.refresh(ctx, refresh, jsonStrings(oauth["scopes"]))
	if err != nil {
		if access != "" {
			return access, nil
		}
		return "", err
	}

	// Persist the rotated token, preserving every other field (including
	// the unknown ones the official client writes, e.g. subscriptionType).
	oauth["accessToken"] = mustJSON(tok.AccessToken)
	if tok.RefreshToken != "" {
		oauth["refreshToken"] = mustJSON(tok.RefreshToken)
	}
	oauth["expiresAt"] = mustJSON(nowMS + tok.ExpiresIn*1000)
	root["claudeAiOauth"] = mustJSON(oauth)
	// A write failure is non-fatal: the in-memory token is still valid for
	// this process; the next process will refresh again.
	_ = writeFileAtomic(s.path, mustJSON(root))

	return tok.AccessToken, nil
}

func (s *Source) refresh(ctx context.Context, refreshToken string, scopes []string) (tokenResponse, error) {
	return exchangeRefreshToken(ctx, s.client, refreshToken, strings.Join(scopes, " "))
}

// exchangeRefreshToken runs the refresh_token grant against the OAuth
// token endpoint. An empty scope falls back to the full subscription set:
// the backend allows scope expansion on refresh, so tokens minted before
// a scope existed can still pick it up.
func exchangeRefreshToken(ctx context.Context, client *http.Client, refreshToken, scope string) (tokenResponse, error) {
	if scope == "" {
		scope = strings.Join(subscriptionScopes, " ")
	}
	body, _ := json.Marshal(map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
		"scope":         scope,
	})
	tr, err := postToken(ctx, client, body)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("claudecode: refresh: %w", err)
	}
	return tr, nil
}

// postToken posts a JSON grant to the OAuth token endpoint and decodes
// the response, rejecting a 200 that carries no access token.
func postToken(ctx context.Context, client *http.Client, body []byte) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return tokenResponse{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return tokenResponse{}, fmt.Errorf("decode response: %w", err)
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("response carried no access token")
	}
	return tr, nil
}

type modelsResponse struct {
	Data []struct {
		ID             string `json:"id"`
		DisplayName    string `json:"display_name"`
		MaxInputTokens int64  `json:"max_input_tokens"`
		MaxTokens      int64  `json:"max_tokens"`
		Capabilities   struct {
			Thinking struct {
				Supported bool `json:"supported"`
			} `json:"thinking"`
			ImageInput struct {
				Supported bool `json:"supported"`
			} `json:"image_input"`
			Effort effortCapabilities `json:"effort"`
		} `json:"capabilities"`
	} `json:"data"`
}

// effortCapabilities mirrors the "capabilities.effort" object of a
// /v1/models response entry: a top-level supported flag plus one flag
// per named effort level.
type effortCapabilities struct {
	Supported bool `json:"supported"`
	Low       struct {
		Supported bool `json:"supported"`
	} `json:"low"`
	Medium struct {
		Supported bool `json:"supported"`
	} `json:"medium"`
	High struct {
		Supported bool `json:"supported"`
	} `json:"high"`
	XHigh struct {
		Supported bool `json:"supported"`
	} `json:"xhigh"`
	Max struct {
		Supported bool `json:"supported"`
	} `json:"max"`
}

// effortLevels returns the reasoning-effort levels this model supports,
// in Anthropic's documented low-to-max order, read directly from the
// live /v1/models response rather than a hardcoded list -- so newly
// released levels (e.g. xhigh, max) appear automatically for whichever
// models actually support them, and are never offered for models that
// don't support a given level. See
// platform.claude.com/docs/en/build-with-claude/effort.
func effortLevels(c effortCapabilities) []string {
	if !c.Supported {
		return nil
	}
	var levels []string
	if c.Low.Supported {
		levels = append(levels, "low")
	}
	if c.Medium.Supported {
		levels = append(levels, "medium")
	}
	if c.High.Supported {
		levels = append(levels, "high")
	}
	if c.XHigh.Supported {
		levels = append(levels, "xhigh")
	}
	if c.Max.Supported {
		levels = append(levels, "max")
	}
	return levels
}

// Models queries /v1/models and returns the subscription's available models
// mapped to Crush's model type.
func (s *Source) Models(ctx context.Context) ([]catwalk.Model, error) {
	token, err := s.Token(ctx)
	if err != nil {
		return nil, err
	}
	base := s.baseURL
	if base == "" {
		base = BaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", OAuthBeta)
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claudecode: list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("claudecode: list models: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var mr modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("claudecode: decode models: %w", err)
	}

	models := make([]catwalk.Model, 0, len(mr.Data))
	for _, m := range mr.Data {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		levels := effortLevels(m.Capabilities.Effort)
		models = append(models, catwalk.Model{
			ID:                     m.ID,
			Name:                   name,
			ContextWindow:          m.MaxInputTokens,
			DefaultMaxTokens:       m.MaxTokens,
			CanReason:              m.Capabilities.Thinking.Supported || len(levels) > 0,
			SupportsImages:         m.Capabilities.ImageInput.Supported,
			ReasoningLevels:        levels,
			DefaultReasoningEffort: defaultEffortLevel(levels),
		})
	}
	return models, nil
}

// defaultEffortLevel picks the effort level Crush should use when the
// user hasn't chosen one. Anthropic documents "high" as the API's own
// default (explicitly equivalent to omitting the effort parameter), so
// that is preferred whenever the model supports it; otherwise the
// highest level the model does support is used.
func defaultEffortLevel(levels []string) string {
	if slices.Contains(levels, "high") {
		return "high"
	}
	if len(levels) > 0 {
		return levels[len(levels)-1]
	}
	return ""
}

type modelsCacheEntry struct {
	models []catwalk.Model
	at     time.Time
}

var (
	modelsCacheMu sync.Mutex
	// modelsCache is keyed by provider id: each subscription account gets
	// its own entry, since two accounts can be on different plans and so
	// see different model line-ups.
	modelsCache = map[string]modelsCacheEntry{}
)

// CachedModels returns the default account's subscription models.
func CachedModels(ctx context.Context) []catwalk.Model {
	return CachedModelsFor(ctx, ProviderID, DefaultSource())
}

// CachedModelsFor returns the subscription models for one account,
// querying the API at most once per hour per provider and falling back to
// the bundled defaults on any error so a provider is never left without
// models (e.g. offline at startup).
func CachedModelsFor(ctx context.Context, providerID string, src *Source) []catwalk.Model {
	modelsCacheMu.Lock()
	defer modelsCacheMu.Unlock()

	cached := modelsCache[providerID]
	if len(cached.models) > 0 && time.Since(cached.at) < time.Hour {
		return cached.models
	}
	if src == nil {
		src = DefaultSource()
	}
	models, err := src.Models(ctx)
	if err != nil || len(models) == 0 {
		if len(cached.models) > 0 {
			return cached.models
		}
		return DefaultModels()
	}
	modelsCache[providerID] = modelsCacheEntry{models: models, at: time.Now()}
	return models
}

// DefaultModels is a static fallback list used when /v1/models cannot be
// reached. It mirrors the known Claude Code subscription line-up.
//
// It intentionally leaves ReasoningLevels/DefaultReasoningEffort unset
// rather than guessing which effort levels (e.g. xhigh, max) each model
// supports: that support is only known live, from /v1/models'
// capabilities.effort field (see effortLevels), and would silently go
// stale here as a hardcoded list. Leaving it unset makes Crush fall
// back to the safe, generic off/low/medium/high thinking-budget picker
// (see config.UsesThinkingBudget) until a live model list is available.
func DefaultModels() []catwalk.Model {
	mk := func(id, name string, ctx, maxTok int64) catwalk.Model {
		return catwalk.Model{
			ID: id, Name: name,
			ContextWindow: ctx, DefaultMaxTokens: maxTok,
			CanReason: true, SupportsImages: true,
		}
	}
	return []catwalk.Model{
		mk("claude-opus-4-8", "Claude Opus 4.8", 1000000, 128000),
		mk("claude-sonnet-4-6", "Claude Sonnet 4.6", 1000000, 128000),
		mk("claude-opus-4-6", "Claude Opus 4.6", 1000000, 128000),
		mk("claude-haiku-4-5-20251001", "Claude Haiku 4.5", 200000, 64000),
		mk("claude-opus-4-5-20251101", "Claude Opus 4.5", 200000, 64000),
		mk("claude-sonnet-4-5-20250929", "Claude Sonnet 4.5", 1000000, 64000),
	}
}

// AuthTransport injects a fresh subscription Bearer token and the OAuth beta
// flag on each outgoing request, removing any stale x-api-key. Refreshing
// per request means long sessions never fail on token expiry.
type AuthTransport struct {
	Base   http.RoundTripper
	Source *Source
}

func (t *AuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	src := t.Source
	if src == nil {
		src = DefaultSource()
	}
	if token, err := src.Token(req.Context()); err == nil {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Del("X-Api-Key")
		if beta := req.Header.Get("anthropic-beta"); beta == "" {
			req.Header.Set("anthropic-beta", OAuthBeta)
		} else if !strings.Contains(beta, OAuthBeta) {
			req.Header.Set("anthropic-beta", OAuthBeta+","+beta)
		}
	}
	return base.RoundTrip(req)
}

// --- small JSON helpers -----------------------------------------------------

func jsonString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func jsonInt(raw json.RawMessage) int64 {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var f float64
	_ = json.Unmarshal(raw, &f)
	return int64(f)
}

func jsonStrings(raw json.RawMessage) []string {
	var s []string
	_ = json.Unmarshal(raw, &s)
	return s
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, 0o600)
	return os.Rename(tmpName, path)
}
