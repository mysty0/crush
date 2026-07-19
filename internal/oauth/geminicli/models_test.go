package geminicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefaultModels verifies the curated model list exposes the expected
// ids and context windows.
func TestDefaultModels(t *testing.T) {
	t.Parallel()

	models := DefaultModels()
	require.Len(t, models, 4)

	byID := make(map[string]int64, len(models))
	for _, m := range models {
		byID[m.ID] = m.ContextWindow
		require.True(t, m.CanReason, "model %s should reason", m.ID)
		require.True(t, m.SupportsImages, "model %s should support images", m.ID)
	}

	require.Equal(t, int64(1048576), byID["gemini-2.5-pro"])
	require.Equal(t, int64(1048576), byID["gemini-2.5-flash"])
	require.Equal(t, int64(1048576), byID["gemini-2.0-flash"])
	require.Equal(t, int64(1048576), byID["gemini-2.5-flash-lite"])
}

// TestModelsSuccess covers the fetchAvailableModels happy path: models
// are decoded from the map, the default agent model sorts first, and a
// disabled model is skipped.
func TestModelsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1internal:fetchAvailableModels", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": {
				"gemini-2.5-flash": {"displayName":"Gemini 2.5 Flash","maxTokens":1048576,"maxOutputTokens":65536,"supportsThinking":true,"supportsImages":true},
				"gemini-2.5-pro": {"displayName":"Gemini 2.5 Pro","maxTokens":2097152,"maxOutputTokens":65536,"supportsThinking":true,"supportsImages":true},
				"gemini-legacy": {"displayName":"Legacy","disabled":true}
			},
			"defaultAgentModelId": "gemini-2.5-pro"
		}`))
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	models, err := Models(context.Background(), "tok", "proj-1", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Len(t, models, 2)

	require.Equal(t, "gemini-2.5-pro", models[0].ID)
	require.Equal(t, int64(2097152), models[0].ContextWindow)
	require.Equal(t, int64(65536), models[0].DefaultMaxTokens)
	require.True(t, models[0].CanReason)
	require.True(t, models[0].SupportsImages)

	require.Equal(t, "gemini-2.5-flash", models[1].ID)
}

// TestModelsDedupesByDisplayName covers the case where Google's model
// catalog returns multiple id aliases sharing one display name (a
// common real-world pattern for stable/preview/dated snapshot variants
// of the same model): only one entry per display name should survive,
// preferring the non-preview, non-beta alias.
func TestModelsDedupesByDisplayName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": {
				"gemini-3.1-flash-lite-preview-11-2025": {"displayName":"Gemini 3.1 Flash Lite","preview":true},
				"gemini-3.1-flash-lite": {"displayName":"Gemini 3.1 Flash Lite"},
				"gemini-flash-lite-latest": {"displayName":"Gemini 3.1 Flash Lite","beta":true},
				"gemini-2.5-pro": {"displayName":"Gemini 2.5 Pro"}
			}
		}`))
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	models, err := Models(context.Background(), "tok", "proj-1", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Len(t, models, 2)

	byName := make(map[string]string, len(models))
	for _, m := range models {
		byName[m.Name] = m.ID
	}
	// The non-preview, non-beta alias wins.
	require.Equal(t, "gemini-3.1-flash-lite", byName["Gemini 3.1 Flash Lite"])
	require.Equal(t, "gemini-2.5-pro", byName["Gemini 2.5 Pro"])
}

// TestModelsErrorReturnsDefaults covers a discovery failure falling back
// to the static model list while still surfacing the error.
func TestModelsErrorReturnsDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	models, err := Models(context.Background(), "tok", "proj-1", GeminiCLIIdentity)
	require.Error(t, err)
	require.Equal(t, DefaultModels(), models)
}

// TestCachedModelsFallsBackOnFailure covers CachedModels falling back to
// the static list on a transport failure while still reporting the
// error, so a caller can surface a "using defaults" warning instead of
// the failure being invisible.
func TestCachedModelsFallsBackOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	withCodeAssistEndpoint(t, url)

	models, err := CachedModels(context.Background(), "tok-cache", "proj-1", GeminiCLIIdentity)
	require.Error(t, err)
	require.Equal(t, DefaultModels(), models)
}
