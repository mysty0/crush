// Package codex implements the OpenAI Codex (ChatGPT Plus/Pro
// subscription) OAuth login, token refresh, and an HTTP transport that
// signs inference requests to the ChatGPT backend.
//
// It supports two login flows: a PKCE loopback (browser) flow bound to
// the fixed redirect URI http://localhost:1455/auth/callback, and a
// device-code flow for headless environments. Both yield an
// *oauth.Token whose access token is a JWT carrying the ChatGPT account
// id and email, so callers can persist just the token and re-derive
// those values on demand.
package codex

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const (
	// ProviderID is the reserved Crush provider id that activates native
	// OpenAI Codex subscription handling.
	ProviderID = "openai-codex"

	// BaseURL is the ChatGPT backend base for Codex inference. Appending
	// "/responses" yields the Codex responses path "/codex/responses".
	BaseURL = "https://chatgpt.com/backend-api/codex"

	// clientID is the OAuth client id registered for the Codex CLI.
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// scope is the set of OAuth scopes requested for a Codex login.
	scope = "openid profile email offline_access api.connectors.read api.connectors.invoke"

	// originator identifies the client on authorization and inference
	// requests.
	originator = "pi"

	// callbackPort is the fixed loopback port for the browser flow.
	// OpenAI rejects any other redirect URI, so port fallback is
	// disallowed.
	callbackPort = 1455

	// callbackPath is the fixed loopback callback path.
	callbackPath = "/auth/callback"

	// deviceRedirectURI is the redirect URI used when exchanging a
	// device-flow authorization code for tokens.
	deviceRedirectURI = "https://auth.openai.com/deviceauth/callback"

	// deviceAuthURL is the page the user opens to enter their device
	// user code.
	deviceAuthURL = "https://auth.openai.com/codex/device"

	// devicePollInterval is the fallback poll cadence for the device
	// flow when the server does not supply one.
	devicePollIntervalSeconds = 5

	// deviceMaxPolls caps how many times the device flow polls before
	// giving up.
	deviceMaxPolls = 120

	// accountClaim is the JWT claim path holding the auth object with
	// chatgpt_account_id.
	accountClaim = "https://api.openai.com/auth"

	// profileClaim is the JWT claim path holding the profile object with
	// email.
	profileClaim = "https://api.openai.com/profile"
)

// Endpoint URLs are package-level vars (not consts) so tests can point
// them at httptest servers.
var (
	// authorizeURL is the OAuth authorization endpoint.
	authorizeURL = "https://auth.openai.com/oauth/authorize"

	// tokenURL is the OAuth token endpoint for code exchange and
	// refresh.
	tokenURL = "https://auth.openai.com/oauth/token"

	// deviceUsercodeURL issues a device user code to begin the device
	// flow.
	deviceUsercodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"

	// deviceTokenURL is polled to complete the device flow.
	deviceTokenURL = "https://auth.openai.com/api/accounts/deviceauth/token"
)

// DecodeAccountID extracts the ChatGPT account id from the given access
// token JWT. It returns "" when the token cannot be decoded or the claim
// is absent.
func DecodeAccountID(accessToken string) string {
	claims := decodeJWTClaims(accessToken)
	if claims == nil {
		return ""
	}
	auth, ok := claims[accountClaim].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := auth["chatgpt_account_id"].(string)
	return id
}

// DecodeEmail extracts the account email from the given access token
// JWT. It returns "" when the token cannot be decoded or the claim is
// absent.
func DecodeEmail(accessToken string) string {
	claims := decodeJWTClaims(accessToken)
	if claims == nil {
		return ""
	}
	// The email lives under the profile claim, but some tokens also
	// carry a top-level "email" claim; prefer the profile object.
	if profile, ok := claims[profileClaim].(map[string]any); ok {
		if email, _ := profile["email"].(string); email != "" {
			return email
		}
	}
	email, _ := claims["email"].(string)
	return email
}

// decodeJWTClaims base64url-decodes the payload (middle) segment of a
// JWT and unmarshals it into a claim map. It tolerates malformed input
// by returning nil.
func decodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}
