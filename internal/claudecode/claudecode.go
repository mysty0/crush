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
	// Claude Code subscription handling (auth + model discovery).
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

// Source reads and refreshes Claude Code OAuth tokens. It is safe for
// concurrent use; refreshes are serialized so a single rotation is shared.
type Source struct {
	mu      sync.Mutex
	path    string
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

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Token returns a valid access token, refreshing and persisting a new one
// when the stored token is missing or within the refresh skew of expiry.
// If a refresh fails but a (possibly stale) token exists, that token is
// returned rather than erroring — the API is the source of truth on
// validity.
func (s *Source) Token(ctx context.Context) (string, error) {
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
	scope := strings.Join(scopes, " ")
	if scope == "" {
		scope = "user:inference user:profile"
	}
	body, _ := json.Marshal(map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
		"scope":         scope,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("claudecode: refresh request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return tokenResponse{}, fmt.Errorf("claudecode: refresh failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return tokenResponse{}, fmt.Errorf("claudecode: decode refresh: %w", err)
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("claudecode: refresh returned empty access token")
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

var (
	modelsCacheMu sync.Mutex
	modelsCache   []catwalk.Model
	modelsCacheAt time.Time
)

// CachedModels returns the subscription models, querying the API at most
// once per hour and falling back to the bundled defaults on any error so a
// provider is never left without models (e.g. offline at startup).
func CachedModels(ctx context.Context) []catwalk.Model {
	modelsCacheMu.Lock()
	defer modelsCacheMu.Unlock()

	if len(modelsCache) > 0 && time.Since(modelsCacheAt) < time.Hour {
		return modelsCache
	}
	models, err := DefaultSource().Models(ctx)
	if err != nil || len(models) == 0 {
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return DefaultModels()
	}
	modelsCache = models
	modelsCacheAt = time.Now()
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
