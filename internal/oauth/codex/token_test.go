package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests reassign the package-level tokenURL var, so they must not
// run in parallel with each other.

func TestExchange(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-xyz"},
	})

	var gotForm map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		gotForm = map[string]string{}
		for k := range r.Form {
			gotForm[k] = r.Form.Get(k)
		}
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  access,
			"refresh_token": "refresh-abc",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	tok, err := Exchange(context.Background(), "the-code", "the-verifier", "http://localhost:1455/auth/callback")
	require.NoError(t, err)
	require.Equal(t, access, tok.AccessToken)
	require.Equal(t, "refresh-abc", tok.RefreshToken)
	require.Positive(t, tok.ExpiresAt)
	require.False(t, tok.IsExpired())

	require.Equal(t, "authorization_code", gotForm["grant_type"])
	require.Equal(t, clientID, gotForm["client_id"])
	require.Equal(t, "the-code", gotForm["code"])
	require.Equal(t, "the-verifier", gotForm["code_verifier"])
	require.Equal(t, "http://localhost:1455/auth/callback", gotForm["redirect_uri"])
}

func TestExchange_MissingAccountID(t *testing.T) {
	access := makeJWT(t, map[string]any{"foo": "bar"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access,
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	_, err := Exchange(context.Background(), "c", "v", "r")
	require.Error(t, err)
}

func TestExchange_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	_, err := Exchange(context.Background(), "c", "v", "r")
	require.Error(t, err)
}

func TestRefreshToken(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-refresh"},
	})

	var gotForm map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		gotForm = map[string]string{}
		for k := range r.Form {
			gotForm[k] = r.Form.Get(k)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  access,
			"refresh_token": "new-refresh",
			"expires_in":    7200,
		})
	}))
	defer srv.Close()

	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	tok, err := RefreshToken(context.Background(), "old-refresh")
	require.NoError(t, err)
	require.Equal(t, access, tok.AccessToken)
	require.Equal(t, "new-refresh", tok.RefreshToken)
	require.Positive(t, tok.ExpiresAt)

	require.Equal(t, "refresh_token", gotForm["grant_type"])
	require.Equal(t, "old-refresh", gotForm["refresh_token"])
	require.Equal(t, clientID, gotForm["client_id"])
}

func TestRefreshToken_PreservesRefreshToken(t *testing.T) {
	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-refresh"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No refresh_token in the response.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access,
			"expires_in":   7200,
		})
	}))
	defer srv.Close()

	old := tokenURL
	tokenURL = srv.URL
	defer func() { tokenURL = old }()

	tok, err := RefreshToken(context.Background(), "keep-me")
	require.NoError(t, err)
	require.Equal(t, "keep-me", tok.RefreshToken)
}

func TestRefreshToken_Empty(t *testing.T) {
	_, err := RefreshToken(context.Background(), "")
	require.Error(t, err)
}
