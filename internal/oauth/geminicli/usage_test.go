package geminicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFetchUsageSuccess covers the happy path where both loadCodeAssist and
// retrieveUserQuota succeed.
func TestFetchUsageSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		require.Contains(t, r.Header.Get("User-Agent"), "GeminiCLI/")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{"currentTier":{"id":"free-tier"}}`))
		case "/v1internal:retrieveUserQuota":
			_, _ = w.Write([]byte(`{"remainingFraction":0.75}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	usage, err := FetchUsage(context.Background(), "tok", "proj")
	require.NoError(t, err)
	require.Equal(t, "free-tier", usage.Tier)
	require.InDelta(t, 0.75, usage.RemainingFraction, 1e-9)
}

// TestFetchUsageQuotaError covers a retrieveUserQuota failure surfacing as
// an error.
func TestFetchUsageQuotaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"currentTier":{"id":"free-tier"}}`))
		case "/v1internal:retrieveUserQuota":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	usage, err := FetchUsage(context.Background(), "tok", "proj")
	require.Error(t, err)
	require.Nil(t, usage)
}

// TestFetchUsageTierErrorQuotaOK covers a loadCodeAssist failure being
// swallowed while the quota still parses.
func TestFetchUsageTierErrorQuotaOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("nope"))
		case "/v1internal:retrieveUserQuota":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"remainingFraction":0.5}}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	usage, err := FetchUsage(context.Background(), "tok", "proj")
	require.NoError(t, err)
	require.Equal(t, "", usage.Tier)
	require.InDelta(t, 0.5, usage.RemainingFraction, 1e-9)
}
