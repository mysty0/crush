package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests reassign the package-level accountAPIBaseURL var, so they
// must not run in parallel with each other.

func TestModels_CodexPath(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-models"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/codex/models", r.URL.Path)
		require.Equal(t, "Bearer "+access, r.Header.Get("Authorization"))
		require.Equal(t, "acct-models", r.Header.Get("chatgpt-account-id"))
		require.Equal(t, "responses=experimental", r.Header.Get("OpenAI-Beta"))
		require.Equal(t, "pi", r.Header.Get("originator"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{
					"slug":                       "gpt-5.1-codex",
					"display_name":               "GPT-5.1 Codex",
					"context_window":             400000,
					"supported_reasoning_levels": []string{"low", "high"},
					"input_modalities":           []string{"text", "image"},
				},
			},
		})
	}))
	defer srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = srv.URL
	defer func() { accountAPIBaseURL = old }()

	models, err := Models(context.Background(), access)
	require.NoError(t, err)
	require.Len(t, models, 1)

	m := models[0]
	require.Equal(t, "gpt-5.1-codex", m.ID)
	require.Equal(t, "GPT-5.1 Codex", m.Name)
	require.Equal(t, int64(400000), m.ContextWindow)
	require.Equal(t, int64(128000), m.DefaultMaxTokens)
	require.True(t, m.CanReason)
	require.True(t, m.SupportsImages)
}

func TestModels_FallsBackToGenericPath(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-generic"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/codex/models":
			http.NotFound(w, r)
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"id":             "gpt-5-codex",
						"display_name":   "GPT-5 Codex",
						"context_window": 200000,
					},
				},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = srv.URL
	defer func() { accountAPIBaseURL = old }()

	models, err := Models(context.Background(), access)
	require.NoError(t, err)
	require.Len(t, models, 1)

	m := models[0]
	require.Equal(t, "gpt-5-codex", m.ID)
	require.Equal(t, "GPT-5 Codex", m.Name)
	require.Equal(t, int64(200000), m.ContextWindow)
	require.Equal(t, int64(128000), m.DefaultMaxTokens)
	require.False(t, m.SupportsImages)
}

func TestModels_SkipsUnsupportedAndSortsByPriority(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-sort"},
	})

	no := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"slug": "third", "priority": 30},
				{"slug": "hidden", "supported_in_api": no},
				{"slug": "first", "priority": 10},
				{"slug": "second", "priority": 20},
				{"slug": "last-no-prio"},
			},
		})
	}))
	defer srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = srv.URL
	defer func() { accountAPIBaseURL = old }()

	models, err := Models(context.Background(), access)
	require.NoError(t, err)
	require.Len(t, models, 4)
	require.Equal(t, "first", models[0].ID)
	require.Equal(t, "second", models[1].ID)
	require.Equal(t, "third", models[2].ID)
	require.Equal(t, "last-no-prio", models[3].ID)
}

func TestModels_ErrorReturnsDefaults(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-err"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = srv.URL
	defer func() { accountAPIBaseURL = old }()

	models, err := Models(context.Background(), access)
	require.Error(t, err)
	require.Equal(t, DefaultModels(), models)
}

func TestCachedModels_FallsBackOnFailure(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-cache"},
	})

	// Point at a closed server so every request fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = url
	defer func() { accountAPIBaseURL = old }()

	models := CachedModels(context.Background(), access)
	require.Equal(t, DefaultModels(), models)
}

func TestDefaultModels(t *testing.T) {
	t.Parallel()

	models := DefaultModels()
	require.Len(t, models, 3)
	for _, m := range models {
		require.NotEmpty(t, m.ID)
		require.NotEmpty(t, m.Name)
		require.Equal(t, int64(272000), m.ContextWindow)
		require.Equal(t, int64(128000), m.DefaultMaxTokens)
		require.True(t, m.CanReason)
		require.True(t, m.SupportsImages)
	}
}
