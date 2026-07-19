package geminicli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

// modelsCacheTTL is how long a successful Models result is reused before
// the discovery endpoint is queried again.
const modelsCacheTTL = 5 * time.Minute

// DefaultModels returns the Gemini models available through the Cloud Code
// Assist (Gemini CLI) subscription.
//
// This curated static list is used only as a fallback when the native
// fetchAvailableModels discovery call (see Models) fails or returns
// nothing usable.
func DefaultModels() []catwalk.Model {
	return []catwalk.Model{
		{
			ID:               "gemini-2.5-pro",
			Name:             "Gemini 2.5 Pro",
			ContextWindow:    1048576,
			DefaultMaxTokens: 65536,
			CanReason:        true,
			SupportsImages:   true,
		},
		{
			ID:               "gemini-2.5-flash",
			Name:             "Gemini 2.5 Flash",
			ContextWindow:    1048576,
			DefaultMaxTokens: 65536,
			CanReason:        true,
			SupportsImages:   true,
		},
		{
			ID:               "gemini-2.0-flash",
			Name:             "Gemini 2.0 Flash",
			ContextWindow:    1048576,
			DefaultMaxTokens: 8192,
			CanReason:        true,
			SupportsImages:   true,
		},
		{
			ID:               "gemini-2.5-flash-lite",
			Name:             "Gemini 2.5 Flash-Lite",
			ContextWindow:    1048576,
			DefaultMaxTokens: 65536,
			CanReason:        true,
			SupportsImages:   true,
		},
	}
}

// modelDetails is a single entry of a fetchAvailableModels response's
// "models" map. Field names and the overall message shape were recovered
// by disassembling the real Antigravity CLI binary's compiled protobuf
// descriptor for google.internal.cloud.code.v1internal.ModelDetails; see
// docs/antigravity-cli-oauth-findings.md. Only the fields Crush uses are
// declared here; the rest are ignored by the JSON decoder.
type modelDetails struct {
	DisplayName      string `json:"displayName"`
	Description      string `json:"description"`
	SupportsImages   bool   `json:"supportsImages"`
	SupportsVideo    bool   `json:"supportsVideo"`
	SupportsThinking bool   `json:"supportsThinking"`
	MaxTokens        int64  `json:"maxTokens"`
	MaxOutputTokens  int64  `json:"maxOutputTokens"`
	Beta             bool   `json:"beta"`
	Disabled         bool   `json:"disabled"`
	Preview          bool   `json:"preview"`
}

// fetchAvailableModelsResponse is the parsed body of a
// v1internal:fetchAvailableModels call. The map key is the model id used
// on every inference request (e.g. "gemini-2.5-pro").
type fetchAvailableModelsResponse struct {
	Models              map[string]modelDetails `json:"models"`
	DefaultAgentModelID string                  `json:"defaultAgentModelId"`
}

// Models fetches the account's available Gemini models from the native
// Cloud Code Assist discovery endpoint (v1internal:fetchAvailableModels),
// falling back to DefaultModels on any error or empty result. projectID
// is the Cloud project discovered via DiscoverProject; the endpoint
// requires it even for free-tier accounts. id identifies the calling
// client product for the required Client-Metadata headers.
func Models(ctx context.Context, accessToken, projectID string, id Identity) ([]catwalk.Model, error) {
	body := map[string]any{"project": projectID}
	respBody, status, err := codeAssistPost(ctx, accessToken, "fetchAvailableModels", body, id)
	if err != nil {
		return DefaultModels(), err
	}
	if status != http.StatusOK {
		return DefaultModels(), fmt.Errorf("geminicli: fetchAvailableModels failed: %s - %s",
			http.StatusText(status), string(respBody))
	}

	var parsed fetchAvailableModelsResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return DefaultModels(), fmt.Errorf("geminicli: parse fetchAvailableModels response: %w", err)
	}

	models := normalizeModels(parsed.Models, parsed.DefaultAgentModelID)
	if len(models) == 0 {
		return DefaultModels(), fmt.Errorf("geminicli: fetchAvailableModels returned no usable models")
	}
	return models, nil
}

// skipping disabled entries. Google's model catalog frequently exposes
// several id aliases for what is effectively the same model (a stable
// id, a preview id, a dated snapshot, etc.) sharing one identical
// display name; normalizeModels collapses each such group down to a
// single picker entry, preferring the non-preview/non-beta alias (per
// the real "preview"/"beta" fields Google's own schema defines) so the
// picker doesn't show the same model name multiple times. The default
// agent model (if present and enabled) sorts first; the rest are sorted
// alphabetically by id for a stable order.
func normalizeModels(entries map[string]modelDetails, defaultID string) []catwalk.Model {
	type candidate struct {
		id string
		d  modelDetails
	}

	byName := make(map[string][]candidate)
	var names []string
	for id, d := range entries {
		if d.Disabled {
			continue
		}
		name := d.DisplayName
		if name == "" {
			name = id
		}
		if _, ok := byName[name]; !ok {
			names = append(names, name)
		}
		byName[name] = append(byName[name], candidate{id: id, d: d})
	}

	var models []catwalk.Model
	for _, name := range names {
		cands := byName[name]
		sort.SliceStable(cands, func(i, j int) bool {
			a, b := cands[i], cands[j]
			switch {
			case a.d.Preview != b.d.Preview:
				return !a.d.Preview
			case a.d.Beta != b.d.Beta:
				return !a.d.Beta
			default:
				return a.id < b.id
			}
		})
		winner := cands[0]

		contextWindow := winner.d.MaxTokens
		if contextWindow <= 0 {
			contextWindow = 1048576
		}
		maxTokens := winner.d.MaxOutputTokens
		if maxTokens <= 0 {
			maxTokens = 65536
		}

		models = append(models, catwalk.Model{
			ID:               winner.id,
			Name:             name,
			ContextWindow:    contextWindow,
			DefaultMaxTokens: maxTokens,
			CanReason:        winner.d.SupportsThinking,
			SupportsImages:   winner.d.SupportsImages,
		})
	}

	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i], models[j]
		switch {
		case a.ID == defaultID && b.ID != defaultID:
			return true
		case a.ID != defaultID && b.ID == defaultID:
			return false
		default:
			return a.ID < b.ID
		}
	})
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

// CachedModels returns Models, always with a usable model list even on
// failure (falling back to DefaultModels), but also returns the error
// so a caller can surface a "using a limited default list" warning
// instead of the failure being invisible. Successful results are
// memoized per access token for a short TTL (see modelsCacheTTL) to
// avoid refetching on every config load.
func CachedModels(ctx context.Context, accessToken, projectID string, id Identity) ([]catwalk.Model, error) {
	now := time.Now()

	modelsCacheMu.Lock()
	if entry, ok := modelsCache[accessToken]; ok && now.Before(entry.expires) {
		models := entry.models
		modelsCacheMu.Unlock()
		return models, nil
	}
	modelsCacheMu.Unlock()

	models, err := Models(ctx, accessToken, projectID, id)
	if err != nil {
		slog.Warn("Gemini CLI live model discovery failed; falling back to the default model list (may be missing newly released models)", "error", err)
		// Models already returns DefaultModels on failure; do not cache
		// the fallback so a later call can retry discovery.
		return models, err
	}

	modelsCacheMu.Lock()
	modelsCache[accessToken] = modelsCacheEntry{models: models, expires: now.Add(modelsCacheTTL)}
	modelsCacheMu.Unlock()

	return models, nil
}
