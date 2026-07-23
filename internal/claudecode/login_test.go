package claudecode

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthorizationURL pins the authorization request. These parameters
// are what make Anthropic treat the login as a Claude Pro/Max
// subscription sign-in rather than a Console API-key grant, so a silent
// change here would mint tokens that cannot run subscription inference.
func TestAuthorizationURL(t *testing.T) {
	t.Parallel()

	raw := authorizationURL("http://localhost:1234/callback", "challenge", "state-value")
	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "https://claude.com/cai/oauth/authorize", u.Scheme+"://"+u.Host+u.Path)

	q := u.Query()
	assert.Equal(t, "true", q.Get("code"), "offers the subscription sign-in, not just Console auth")
	assert.Equal(t, oauthClientID, q.Get("client_id"))
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "http://localhost:1234/callback", q.Get("redirect_uri"))
	assert.Equal(t, "challenge", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "state-value", q.Get("state"))

	scopes := q.Get("scope")
	assert.Contains(t, scopes, "user:inference", "without this the token cannot run inference")
	assert.Contains(t, scopes, "user:profile")
	assert.Contains(t, scopes, "user:sessions:claude_code")
}

// TestAuthorizationURLManualRedirect covers the headless flow, which
// swaps the loopback listener for Anthropic's hosted redirect page.
func TestAuthorizationURLManualRedirect(t *testing.T) {
	t.Parallel()

	u, err := url.Parse(authorizationURL(manualRedirectURI, "challenge", "state-value"))
	require.NoError(t, err)
	assert.Equal(t, manualRedirectURI, u.Query().Get("redirect_uri"))
}
