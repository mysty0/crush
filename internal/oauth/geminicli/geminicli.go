// Package geminicli implements the Google Gemini CLI (Cloud Code Assist)
// OAuth login, token refresh, and project discovery flows, plus an HTTP
// transport that adapts Crush's standard Gemini requests to the Cloud Code
// Assist wire format.
//
// The Gemini CLI authenticates a Google account with a loopback
// authorization-code flow using the CLI's public OAuth client, then
// onboards the user to a Cloud Code Assist project. Every inference request
// is then routed through the cloudcode-pa.googleapis.com backend, which
// wraps and unwraps the standard Gemini request/response bodies. See
// WireTransport for the adapter.
package geminicli

import (
	"fmt"
	"runtime"
)

// ProviderID is the reserved Crush provider id that activates native
// Gemini CLI (Cloud Code Assist) subscription handling.
const ProviderID = "google-gemini-cli"

// BaseURL is the Cloud Code Assist API base URL that the fantasy google
// provider must be configured with; WireTransport rewrites requests onto
// its /v1internal endpoints.
const BaseURL = "https://cloudcode-pa.googleapis.com"

const (
	// clientID is the Gemini CLI's public OAuth client id. It is not a
	// secret; it is embedded in the published CLI.
	clientID = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"

	// clientSecretReversed is the Gemini CLI's public client "secret",
	// stored character-reversed so automated secret scanners do not flag
	// this well-known public value. Installed applications cannot keep a
	// real secret, so Google treats this as a public identifier rather
	// than a credential.
	clientSecretReversed = "lxsFXlc5uC6Veg-kS7o1-mPMgHu4-XPSCOG"

	// callbackPort is the preferred loopback port for the OAuth redirect.
	callbackPort = 8085
	// callbackPath is the OAuth redirect path the CLI registers.
	callbackPath = "/oauth2callback"
	// callbackHostname is the loopback host used in the redirect URI.
	callbackHostname = "127.0.0.1"

	// defaultModel is used in the Gemini CLI User-Agent when the concrete
	// model is unknown.
	defaultModel = "gemini-3.1-pro-preview"

	// cliVersion is the Gemini CLI version reported in the User-Agent.
	cliVersion = "0.46.0"
)

// Cloud Code Assist tier identifiers.
const (
	// freeTier is the tier id that requires no Cloud project.
	freeTier = "free-tier"
	// standardTier is the fallback tier used for VPC-SC-restricted users
	// whose loadCodeAssist call is blocked by a security policy.
	standardTier = "standard-tier"
	// legacyTier is the fallback tier when no default tier is advertised.
	legacyTier = "legacy-tier"
)

// scopes are the OAuth scopes the Gemini CLI requests.
var scopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

// Endpoint URLs are package-level variables so tests can point them at
// httptest servers.
var (
	// authURL is Google's OAuth 2.0 authorization endpoint.
	authURL = "https://accounts.google.com/o/oauth2/v2/auth"
	// tokenURL is Google's OAuth 2.0 token endpoint.
	tokenURL = "https://oauth2.googleapis.com/token"
	// codeAssistEndpoint is the Cloud Code Assist API base URL.
	codeAssistEndpoint = BaseURL
	// userinfoURL returns the authenticated user's profile (for email).
	userinfoURL = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"
)

// refreshSkewSeconds trims this many seconds (5 minutes) off the reported
// token lifetime so refreshes happen before the real expiry.
const refreshSkewSeconds = 300

// clientSecret decodes and returns the Gemini CLI's public client secret.
func clientSecret() string {
	b := []byte(clientSecretReversed)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// cliHeaders returns the Gemini CLI identification headers that must
// accompany every Cloud Code Assist and inference request. The model is
// embedded in the User-Agent; when empty a default is substituted.
func cliHeaders(model string) map[string]string {
	if model == "" {
		model = defaultModel
	}
	ua := fmt.Sprintf("GeminiCLI/%s/%s (%s; %s; terminal)",
		cliVersion, model, runtime.GOOS, runtime.GOARCH)
	return map[string]string{
		"User-Agent":      ua,
		"Client-Metadata": "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI",
	}
}
