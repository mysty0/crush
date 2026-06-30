package claudecode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeTestCredentials(t *testing.T, accessToken string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	creds := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  accessToken,
			"refreshToken": "refresh-token",
			// Far in the future so Token() returns it without refreshing.
			"expiresAt": time.Now().Add(time.Hour).UnixMilli(),
			"scopes":    []string{"user:inference", "user:profile"},
		},
	}
	data, err := json.Marshal(creds)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	t.Setenv("CLAUDE_CREDENTIALS", path)
}

func TestUsageParsesUtilization(t *testing.T) {
	writeTestCredentials(t, "test-access-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/oauth/usage", r.URL.Path)
		require.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))
		require.Equal(t, OAuthBeta, r.Header.Get("anthropic-beta"))
		require.Equal(t, anthropicVersion, r.Header.Get("anthropic-version"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 42.5, "resets_at": "2026-06-30T15:00:00Z"},
			"seven_day": {"utilization": 10, "resets_at": "2026-07-05T00:00:00Z"},
			"seven_day_opus": {"utilization": null, "resets_at": null},
			"extra_usage": {"is_enabled": true, "monthly_limit": 100, "used_credits": 25, "utilization": 25}
		}`))
	}))
	defer srv.Close()

	s := NewSource()
	s.baseURL = srv.URL

	u, err := s.Usage(context.Background())
	require.NoError(t, err)
	require.NotNil(t, u)

	require.NotNil(t, u.FiveHour)
	require.NotNil(t, u.FiveHour.Utilization)
	require.InDelta(t, 42.5, *u.FiveHour.Utilization, 0.001)
	require.NotNil(t, u.FiveHour.ResetsAt)
	require.Equal(t, "2026-06-30T15:00:00Z", *u.FiveHour.ResetsAt)

	require.NotNil(t, u.SevenDay)
	require.NotNil(t, u.SevenDay.Utilization)
	require.InDelta(t, 10, *u.SevenDay.Utilization, 0.001)

	// Null windows decode to a non-nil RateLimit with nil fields.
	require.NotNil(t, u.SevenDayOpus)
	require.Nil(t, u.SevenDayOpus.Utilization)
	require.Nil(t, u.SevenDayOpus.ResetsAt)

	require.NotNil(t, u.ExtraUsage)
	require.True(t, u.ExtraUsage.IsEnabled)
	require.NotNil(t, u.ExtraUsage.UsedCredits)
	require.InDelta(t, 25, *u.ExtraUsage.UsedCredits, 0.001)

	// Absent windows stay nil.
	require.Nil(t, u.SevenDaySonnet)
}

func TestUsageNonOKStatusErrors(t *testing.T) {
	writeTestCredentials(t, "test-access-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}))
	defer srv.Close()

	s := NewSource()
	s.baseURL = srv.URL

	_, err := s.Usage(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")
}
