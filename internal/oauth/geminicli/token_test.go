package geminicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// withTokenURL overrides tokenURL for the duration of a test.
func withTokenURL(t *testing.T, u string) {
	t.Helper()
	prev := tokenURL
	tokenURL = u
	t.Cleanup(func() { tokenURL = prev })
}

func TestExchangeCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "authorization_code", r.Form.Get("grant_type"))
		require.Equal(t, "the-code", r.Form.Get("code"))
		require.Equal(t, clientID, r.Form.Get("client_id"))
		require.NotEmpty(t, r.Form.Get("client_secret"))
		require.Equal(t, "http://127.0.0.1:8085/oauth2callback", r.Form.Get("redirect_uri"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
	}))
	defer srv.Close()
	withTokenURL(t, srv.URL)

	tok, err := exchangeCode(context.Background(), "the-code", "http://127.0.0.1:8085/oauth2callback")
	require.NoError(t, err)
	require.Equal(t, "at", tok.AccessToken)
	require.Equal(t, "rt", tok.RefreshToken)
	// 3600 - 300 skew.
	require.Equal(t, 3300, tok.ExpiresIn)
	require.False(t, tok.IsExpired())
}

func TestExchangeCodeMissingRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
	}))
	defer srv.Close()
	withTokenURL(t, srv.URL)

	_, err := exchangeCode(context.Background(), "c", "r")
	require.Error(t, err)
	require.Contains(t, err.Error(), "refresh token")
}

func TestRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		require.Equal(t, "old-refresh", r.Form.Get("refresh_token"))
		_, _ = w.Write([]byte(`{"access_token":"new-at","expires_in":1800}`))
	}))
	defer srv.Close()
	withTokenURL(t, srv.URL)

	tok, err := RefreshToken(context.Background(), "old-refresh", "proj-123")
	require.NoError(t, err)
	require.Equal(t, "new-at", tok.AccessToken)
	// No new refresh token returned; the old one is retained.
	require.Equal(t, "old-refresh", tok.RefreshToken)
	require.Equal(t, 1500, tok.ExpiresIn)
}

func TestRefreshTokenRotates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"rotated","expires_in":1800}`))
	}))
	defer srv.Close()
	withTokenURL(t, srv.URL)

	tok, err := RefreshToken(context.Background(), "old-refresh", "proj")
	require.NoError(t, err)
	require.Equal(t, "rotated", tok.RefreshToken)
}

func TestRefreshTokenEmpty(t *testing.T) {
	t.Parallel()
	_, err := RefreshToken(context.Background(), "", "proj")
	require.Error(t, err)
}

func TestBuildAuthURL(t *testing.T) {
	t.Parallel()

	raw := buildAuthURL("http://127.0.0.1:8085/oauth2callback", "state-xyz")
	u, err := url.Parse(raw)
	require.NoError(t, err)
	q := u.Query()
	require.Equal(t, clientID, q.Get("client_id"))
	require.Equal(t, "code", q.Get("response_type"))
	require.Equal(t, "http://127.0.0.1:8085/oauth2callback", q.Get("redirect_uri"))
	require.Equal(t, "offline", q.Get("access_type"))
	require.Equal(t, "consent", q.Get("prompt"))
	require.Equal(t, "state-xyz", q.Get("state"))
	require.Contains(t, q.Get("scope"), "cloud-platform")
}
