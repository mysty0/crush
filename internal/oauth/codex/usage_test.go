package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests reassign the package-level accountAPIBaseURL var, so they
// must not run in parallel with each other.

func TestFetchUsage(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-usage"},
	})

	resetAt := float64(1_700_000_000) // seconds since the Unix epoch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/wham/usage", r.URL.Path)
		require.Equal(t, "Bearer "+access, r.Header.Get("Authorization"))
		require.Equal(t, "acct-usage", r.Header.Get("chatgpt-account-id"))
		require.Equal(t, "responses=experimental", r.Header.Get("OpenAI-Beta"))
		require.Equal(t, "codex_cli_rs", r.Header.Get("originator"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"allowed":       true,
				"limit_reached": false,
				"primary_window": map[string]any{
					"used_percent":         42,
					"limit_window_seconds": 18000,
					"reset_at":             resetAt,
				},
				"secondary_window": map[string]any{
					"used_percent":        90,
					"reset_after_seconds": 3600,
				},
			},
		})
	}))
	defer srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = srv.URL
	defer func() { accountAPIBaseURL = old }()

	usage, err := FetchUsage(context.Background(), access)
	require.NoError(t, err)
	require.Equal(t, "pro", usage.PlanType)
	require.True(t, usage.Allowed)
	require.False(t, usage.LimitReached)

	require.NotNil(t, usage.Primary)
	require.Equal(t, float64(42), usage.Primary.UsedPercent)
	require.Equal(t, int64(18000), usage.Primary.WindowSeconds)
	require.Equal(t, time.Unix(int64(resetAt), 0), usage.Primary.ResetsAt)

	require.NotNil(t, usage.Secondary)
	require.Equal(t, float64(90), usage.Secondary.UsedPercent)
	require.Equal(t, int64(0), usage.Secondary.WindowSeconds)
	require.False(t, usage.Secondary.ResetsAt.IsZero())
}

func TestFetchUsage_NoRateLimit(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-plain"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"plan_type": "plus"})
	}))
	defer srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = srv.URL
	defer func() { accountAPIBaseURL = old }()

	usage, err := FetchUsage(context.Background(), access)
	require.NoError(t, err)
	require.Equal(t, "plus", usage.PlanType)
	require.Nil(t, usage.Primary)
	require.Nil(t, usage.Secondary)
}

func TestFetchUsage_ResetAtMilliseconds(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-ms"},
	})

	resetMs := float64(1_700_000_000_000) // milliseconds since the epoch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent": 150, // clamped to 100.
					"reset_at":     resetMs,
				},
			},
		})
	}))
	defer srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = srv.URL
	defer func() { accountAPIBaseURL = old }()

	usage, err := FetchUsage(context.Background(), access)
	require.NoError(t, err)
	require.NotNil(t, usage.Primary)
	require.Equal(t, float64(100), usage.Primary.UsedPercent)
	require.Equal(t, time.UnixMilli(int64(resetMs)), usage.Primary.ResetsAt)
}

func TestFetchUsage_ServerError(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-err"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	old := accountAPIBaseURL
	accountAPIBaseURL = srv.URL
	defer func() { accountAPIBaseURL = old }()

	_, err := FetchUsage(context.Background(), access)
	require.Error(t, err)
}
