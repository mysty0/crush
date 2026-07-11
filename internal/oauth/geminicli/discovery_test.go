package geminicli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// withCodeAssistEndpoint overrides codeAssistEndpoint for a test.
func withCodeAssistEndpoint(t *testing.T, u string) {
	t.Helper()
	prev := codeAssistEndpoint
	codeAssistEndpoint = u
	t.Cleanup(func() { codeAssistEndpoint = prev })
}

// TestDiscoverProjectCurrentTier covers the path where loadCodeAssist
// returns a current tier and a project directly, so no onboarding runs.
func TestDiscoverProjectCurrentTier(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT_ID", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1internal:loadCodeAssist", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		require.Contains(t, r.Header.Get("User-Agent"), "GeminiCLI/")
		require.Contains(t, r.Header.Get("Client-Metadata"), "pluginType=GEMINI")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj-direct","currentTier":{"id":"standard-tier"}}`))
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	proj, err := DiscoverProject(context.Background(), "tok", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Equal(t, "proj-direct", proj)
}

// TestDiscoverProjectOnboard covers the onboarding path: no current tier,
// a default tier is selected, and onboardUser returns a project via a done
// long-running operation.
func TestDiscoverProjectOnboard(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT_ID", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`))
		case "/v1internal:onboardUser":
			body, _ := io.ReadAll(r.Body)
			var got map[string]any
			require.NoError(t, json.Unmarshal(body, &got))
			require.Equal(t, "free-tier", got["tierId"])
			_, _ = w.Write([]byte(`{"done":true,"response":{"cloudaicompanionProject":{"id":"onboarded-proj"}}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	proj, err := DiscoverProject(context.Background(), "tok", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Equal(t, "onboarded-proj", proj)
}

// TestDiscoverProjectSecurityPolicy covers a VPC-SC user whose
// loadCodeAssist is blocked; it is treated as standard tier.
func TestDiscoverProjectSecurityPolicy(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-proj")
	t.Setenv("GOOGLE_CLOUD_PROJECT_ID", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1internal:loadCodeAssist", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"details":[{"reason":"SECURITY_POLICY_VIOLATED"}]}}`))
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	proj, err := DiscoverProject(context.Background(), "tok", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Equal(t, "env-proj", proj)
}

// TestDiscoverProjectOnboardPolls covers a long-running operation that is
// not immediately done and must be polled.
func TestDiscoverProjectOnboardPolls(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT_ID", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`))
		case r.URL.Path == "/v1internal:onboardUser":
			_, _ = w.Write([]byte(`{"name":"operations/abc","done":false}`))
		case strings.HasPrefix(r.URL.Path, "/v1internal/operations/abc"):
			_, _ = w.Write([]byte(`{"name":"operations/abc","done":true,"response":{"cloudaicompanionProject":{"id":"polled-proj"}}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	// Shorten the poll interval so the test does not wait the full 5s.
	prev := pollInterval
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() { pollInterval = prev })

	proj, err := DiscoverProject(context.Background(), "tok", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Equal(t, "polled-proj", proj)
}
