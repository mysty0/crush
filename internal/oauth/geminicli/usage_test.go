package geminicli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFetchUsageSuccess covers the happy path where both loadCodeAssist and
// retrieveUserQuotaSummary succeed, verifying the required "project" field
// (not "cloudaicompanionProject") is sent and buckets are flattened out of
// their groups with a group-qualified label.
func TestFetchUsageSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		require.Contains(t, r.Header.Get("User-Agent"), "GeminiCLI/")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{"currentTier":{"id":"free-tier"}}`))
		case "/v1internal:retrieveUserQuotaSummary":
			body := readBody(t, r)
			require.Equal(t, "proj", body["project"])
			require.NotContains(t, body, "cloudaicompanionProject")
			_, _ = w.Write([]byte(`{
				"groups": [
					{
						"displayName": "Gemini",
						"buckets": [
							{"displayName": "Gemini 2.5 Pro", "window": "24h", "remainingFraction": 0.75, "resetTime": "2026-01-01T00:00:00Z"},
							{"displayName": "Gemini 2.5 Flash", "window": "24h", "disabled": true}
						]
					}
				]
			}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	usage, err := FetchUsage(context.Background(), "tok", "proj", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Equal(t, "free-tier", usage.Tier)
	require.Len(t, usage.Buckets, 2)

	b := usage.Buckets[0]
	require.Equal(t, "Gemini — Gemini 2.5 Pro", b.Label)
	require.Equal(t, "24h", b.Window)
	require.InDelta(t, 0.75, b.RemainingFraction, 1e-9)
	require.False(t, b.ResetsAt.IsZero())
	require.False(t, b.Disabled)

	require.True(t, usage.Buckets[1].Disabled)
}

// TestFetchUsageDisambiguatesSharedWindowNames is a regression test for
// buckets sharing a generic window-based display name (e.g. "5h",
// "Weekly") across multiple groups: without combining the group's name
// into the label, two different groups' "5h" buckets would render as
// two indistinguishable duplicate "5h" entries.
func TestFetchUsageDisambiguatesSharedWindowNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{}`))
		case "/v1internal:retrieveUserQuotaSummary":
			_, _ = w.Write([]byte(`{
				"groups": [
					{"displayName": "Gemini 2.5 Pro", "buckets": [
						{"displayName": "5h", "remainingFraction": 0.9},
						{"displayName": "Weekly", "remainingFraction": 0.5}
					]},
					{"displayName": "Gemini 2.5 Flash", "buckets": [
						{"displayName": "5h", "remainingFraction": 0.4},
						{"displayName": "Weekly", "remainingFraction": 0.2}
					]}
				]
			}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	usage, err := FetchUsage(context.Background(), "tok", "proj", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Len(t, usage.Buckets, 4)

	labels := make([]string, len(usage.Buckets))
	for i, b := range usage.Buckets {
		labels[i] = b.Label
	}
	require.ElementsMatch(t, []string{
		"Gemini 2.5 Pro — 5h", "Gemini 2.5 Pro — Weekly",
		"Gemini 2.5 Flash — 5h", "Gemini 2.5 Flash — Weekly",
	}, labels, "every label must be unique despite sharing generic window-based bucket names")
}

// TestFetchUsageQuotaError covers a retrieveUserQuotaSummary failure
// surfacing as an error.
func TestFetchUsageQuotaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"currentTier":{"id":"free-tier"}}`))
		case "/v1internal:retrieveUserQuotaSummary":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	usage, err := FetchUsage(context.Background(), "tok", "proj", GeminiCLIIdentity)
	require.Error(t, err)
	require.Nil(t, usage)
}

// TestFetchUsageTierErrorQuotaOK covers a loadCodeAssist failure being
// swallowed while the quota summary still parses.
func TestFetchUsageTierErrorQuotaOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("nope"))
		case "/v1internal:retrieveUserQuotaSummary":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"groups":[{"buckets":[{"displayName":"Gemini 2.5 Flash","remainingFraction":0.5}]}]}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	usage, err := FetchUsage(context.Background(), "tok", "proj", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Equal(t, "", usage.Tier)
	require.Len(t, usage.Buckets, 1)
	require.InDelta(t, 0.5, usage.Buckets[0].RemainingFraction, 1e-9)
}

// TestFetchUsageBucketWithoutFractionIsUnknown covers a bucket that
// reports neither remainingFraction nor a raw remainingAmount, which
// should surface as an unknown (-1) fraction rather than 0.
func TestFetchUsageBucketWithoutFractionIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{}`))
		case "/v1internal:retrieveUserQuotaSummary":
			_, _ = w.Write([]byte(`{"groups":[{"displayName":"Gemini","buckets":[{"window":"24h"}]}]}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withCodeAssistEndpoint(t, srv.URL)

	usage, err := FetchUsage(context.Background(), "tok", "proj", GeminiCLIIdentity)
	require.NoError(t, err)
	require.Len(t, usage.Buckets, 1)
	b := usage.Buckets[0]
	require.Equal(t, "Gemini", b.Label, "falls back to the group's display name when the bucket has none")
	require.InDelta(t, -1.0, b.RemainingFraction, 1e-9)
}

// TestCombineLabel covers the label-disambiguation rule directly.
func TestCombineLabel(t *testing.T) {
	require.Equal(t, "Gemini 2.5 Pro", combineLabel("Gemini 2.5 Pro", ""))
	require.Equal(t, "Gemini 2.5 Pro", combineLabel("", "Gemini 2.5 Pro"))
	require.Equal(t, "Gemini 2.5 Pro", combineLabel("Gemini 2.5 Pro", "Gemini 2.5 Pro"))
	require.Equal(t, "Gemini 2.5 Pro — 5h", combineLabel("Gemini 2.5 Pro", "5h"))
	require.Equal(t, "", combineLabel("", ""))
}

// readBody decodes a request's JSON body into a generic map for field
// assertions.
func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
	return body
}
