package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

// accountAPIBaseURL is the ChatGPT account API base. It differs from
// BaseURL in that it omits the trailing "/codex": the native model-list
// and usage endpoints live directly under "/backend-api". It is a
// package-level var so tests can point it at an httptest server.
var accountAPIBaseURL = "https://chatgpt.com/backend-api"

const (
	// defaultContextWindow is the fallback context length used when a
	// discovered model does not advertise one.
	defaultContextWindow int64 = 272000

	// defaultMaxTokensCap bounds DefaultMaxTokens regardless of how
	// large the context window is.
	defaultMaxTokensCap int64 = 128000

	// modelsCacheTTL is how long a successful CachedModels result is
	// reused before the discovery endpoint is queried again.
	modelsCacheTTL = 5 * time.Minute
)

// accountHTTPClient is the shared client used for native account-API
// requests. Its timeout guards against a hung ChatGPT backend.
var accountHTTPClient = &http.Client{Timeout: 30 * time.Second}

// newAccountAPIRequest builds a GET request to the given account-API URL
// with the standard Codex authentication and content-negotiation
// headers. The chatgpt-account-id header is only set when a non-empty id
// can be derived from the access token.
func newAccountAPIRequest(ctx context.Context, url, accessToken string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerAuthorization, "Bearer "+accessToken)
	if accountID := DecodeAccountID(accessToken); accountID != "" {
		req.Header.Set(headerAccountID, accountID)
	}
	req.Header.Set(headerOpenAIBeta, openAIBetaResponses)
req.Header.Set(headerOriginator, originator)
req.Header.Set("User-Agent", "codex_cli_rs/1.0.0 (Linux; x86_64)")
req.Header.Set("Accept", "application/json")
	return req, nil
}

// modelEntry is a single model as returned by the native Codex model
// discovery endpoint. Every field is optional.
type modelEntry struct {
	Slug                     string   `json:"slug"`
	ID                       string   `json:"id"`
	DisplayName              string   `json:"display_name"`
	ContextWindow            int64    `json:"context_window"`
	SupportedInAPI           *bool    `json:"supported_in_api"`
	Priority                 *float64 `json:"priority"`
SupportedReasoningLevels []any `json:"supported_reasoning_levels"`
	DefaultReasoningLevel    string   `json:"default_reasoning_level"`
	InputModalities          []string `json:"input_modalities"`
}

// modelsResponse is the discovery endpoint payload, which may carry the
// model list under either "models" or "data".
type modelsResponse struct {
	Models []modelEntry `json:"models"`
	Data   []modelEntry `json:"data"`
}

// DefaultModels returns the static fallback Codex model list used when
// the discovery endpoint cannot be reached.
func DefaultModels() []catwalk.Model {
	newModel := func(id, name string) catwalk.Model {
		return catwalk.Model{
			ID:               id,
			Name:             name,
			ContextWindow:    defaultContextWindow,
			DefaultMaxTokens: defaultMaxTokensCap,
			CanReason:        true,
			SupportsImages:   true,
		}
	}
	return []catwalk.Model{
		newModel("gpt-5.1-codex", "GPT-5.1 Codex"),
		newModel("gpt-5.1-codex-mini", "GPT-5.1 Codex Mini"),
		newModel("gpt-5-codex", "GPT-5 Codex"),
	}
}

// Models fetches the account's available Codex models from the native
// discovery endpoint, falling back to DefaultModels on any error or empty
// result. The account id used for the request header is derived from the
// access token via DecodeAccountID.
func Models(ctx context.Context, accessToken string) ([]catwalk.Model, error) {
	// Try the Codex-scoped path first, then the generic path; the first
	// success wins.
urls := []string{
accountAPIBaseURL + "/codex/models?client_version=1.0.0",
accountAPIBaseURL + "/models",
}

	var lastErr error
for _, url := range urls {
entries, err := fetchModels(ctx, url, accessToken)
if err != nil {
lastErr = err
continue
}
		models := normalizeModels(entries)
		if len(models) == 0 {
			lastErr = fmt.Errorf("codex: model discovery at %s returned no usable models", url)
			continue
		}
		return models, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("codex: model discovery returned no models")
	}
	return DefaultModels(), lastErr
}

// fetchModels performs a single GET against the given discovery URL and
// returns the raw model entries. It returns an error on transport
// failure, a non-200 status, or an unparseable body.
func fetchModels(ctx context.Context, url, accessToken string) ([]modelEntry, error) {
	req, err := newAccountAPIRequest(ctx, url, accessToken)
	if err != nil {
		return nil, err
	}

	resp, err := accountHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex: model discovery request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codex: reading model discovery response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex: model discovery at %s: %s - %s",
			url, http.StatusText(resp.StatusCode), strings.TrimSpace(string(body)))
	}

	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("codex: parsing model discovery response: %w", err)
	}

	entries := parsed.Models
	if len(entries) == 0 {
		entries = parsed.Data
	}
	return entries, nil
}

// normalizeModels converts raw discovery entries into catwalk models,
// skipping entries without an id and those explicitly excluded from the
// API. The result is sorted by ascending priority, with entries lacking a
// priority sorted last (stable by original order).
func normalizeModels(entries []modelEntry) []catwalk.Model {
	type ranked struct {
		model    catwalk.Model
		priority float64
		hasPrio  bool
		order    int
	}

	var ranks []ranked
	for i, e := range entries {
		// Skip models the account cannot use via the API.
		if e.SupportedInAPI != nil && !*e.SupportedInAPI {
			continue
		}

		id := e.Slug
		if id == "" {
			id = e.ID
		}
		if id == "" {
			continue
		}

		name := e.DisplayName
		if name == "" {
			name = id
		}

		contextWindow := e.ContextWindow
		if contextWindow <= 0 {
			contextWindow = defaultContextWindow
		}

		maxTokens := defaultMaxTokensCap
		if contextWindow < maxTokens {
			maxTokens = contextWindow
		}

		supportsImages := false
		for _, m := range e.InputModalities {
			if strings.EqualFold(m, "image") {
				supportsImages = true
				break
			}
		}

		r := ranked{
			model: catwalk.Model{
				ID:               id,
				Name:             name,
				ContextWindow:    contextWindow,
				DefaultMaxTokens: maxTokens,
				// Codex models reason; keep this true regardless of
				// whether the entry advertises reasoning levels.
				CanReason:      true,
				SupportsImages: supportsImages,
			},
			order: i,
		}
		if e.Priority != nil {
			r.priority = *e.Priority
			r.hasPrio = true
		}
		ranks = append(ranks, r)
	}

	sort.SliceStable(ranks, func(i, j int) bool {
		a, b := ranks[i], ranks[j]
		switch {
		case a.hasPrio && b.hasPrio:
			return a.priority < b.priority
		case a.hasPrio != b.hasPrio:
			// Entries with a priority sort before those without.
			return a.hasPrio
		default:
			return a.order < b.order
		}
	})

	models := make([]catwalk.Model, 0, len(ranks))
	for _, r := range ranks {
		models = append(models, r.model)
	}
	return models
}

// modelsCacheEntry is a memoized model list with its expiry time.
type modelsCacheEntry struct {
	models  []catwalk.Model
	expires time.Time
}

var (
	modelsCacheMu sync.Mutex
	modelsCache   = map[string]modelsCacheEntry{}
)

// CachedModels returns Models but never fails: on error it logs at debug
// level and returns DefaultModels. Successful results are memoized per
// access token for a short TTL (see modelsCacheTTL) to avoid refetching
// on every config load.
func CachedModels(ctx context.Context, accessToken string) ([]catwalk.Model, error) {
	now := time.Now()

	modelsCacheMu.Lock()
	if entry, ok := modelsCache[accessToken]; ok && now.Before(entry.expires) {
		models := entry.models
		modelsCacheMu.Unlock()
		return models, nil
	}
	modelsCacheMu.Unlock()

	models, err := Models(ctx, accessToken)
	if err != nil {
		slog.Warn("Codex live model discovery failed; falling back to the default model list (may be missing newly released models)", "error", err)
		// Models already returns DefaultModels on failure; do not cache
		// the fallback so a later call can retry discovery.
		return models, err
	}

	modelsCacheMu.Lock()
	modelsCache[accessToken] = modelsCacheEntry{models: models, expires: now.Add(modelsCacheTTL)}
	modelsCacheMu.Unlock()

	return models, nil
}
