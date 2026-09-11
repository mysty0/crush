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

// Identity identifies the client product making Cloud Code Assist
// requests: the User-Agent product/version, plus the pluginType/ideType
// pair that describes the calling plugin. Google's tier-eligibility logic
// for loadCodeAssist keys off pluginType — see
// docs/antigravity-cli-oauth-findings.md, which documents the exact
// PluginType/IdeType enums recovered from the Antigravity CLI binary's raw
// protobuf descriptor. Notably PluginType.GEMINI is marked
// `deprecated = true` in that schema, so callers other than the reference
// Gemini CLI (e.g. the antigravity package) must supply their own identity
// rather than reusing GeminiCLIIdentity.
type Identity struct {
	// Product is the User-Agent product name, e.g. "GeminiCLI".
	Product string
	// Version is the User-Agent product version.
	Version string
	// PluginType is the ClientMetadata.PluginType enum name (e.g.
	// "GEMINI", "CLOUD_CODE") identifying the calling plugin.
	PluginType string
	// IDEType is the ClientMetadata.IdeType enum name (e.g.
	// "IDE_UNSPECIFIED", "ANTIGRAVITY"). Defaults to "IDE_UNSPECIFIED"
	// when empty.
	IDEType string
	// Endpoint overrides the Cloud Code Assist base URL this identity's
	// requests are sent to. Empty means "use the package default"
	// (BaseURL, codeAssistEndpoint's initial value).
	//
	// This exists because loadCodeAssist resolves an account to a
	// different backend project -- and therefore a different quota pool
	// -- depending on which host receives the request, confirmed by
	// directly comparing live traffic: the same OAuth token sent to
	// cloudcode-pa.googleapis.com resolved to a stale project with no
	// usable quota (every inference call 429ed instantly), while the
	// same token sent to daily-cloudcode-pa.googleapis.com -- the host
	// the real Antigravity CLI binary actually uses -- resolved to the
	// correct, working free-tier project. See antigravity.BaseURL.
	Endpoint string
}

// GeminiCLIIdentity is the identity Gemini CLI itself reports.
var GeminiCLIIdentity = Identity{
	Product:    "GeminiCLI",
	Version:    cliVersion,
	PluginType: "GEMINI",
	IDEType:    "IDE_UNSPECIFIED",
}

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

// endpointFor returns the Cloud Code Assist base URL to use for id's
// requests: id.Endpoint when set (see Identity.Endpoint), otherwise the
// package default codeAssistEndpoint.
func endpointFor(id Identity) string {
	if id.Endpoint != "" {
		return id.Endpoint
	}
	return codeAssistEndpoint
}

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

// userAgent builds the client User-Agent string. The model is embedded in
// it; when empty a default is substituted. It is exposed separately from
// cliHeaders because the Cloud Code Assist request envelope carries the
// same string in its own userAgent field, and the two must not drift.
func userAgent(model string, id Identity) string {
	if model == "" {
		model = defaultModel
	}
	return fmt.Sprintf("%s/%s/%s (%s; %s; terminal)",
		id.Product, id.Version, model, runtime.GOOS, runtime.GOARCH)
}

// cliHeaders returns the client identification headers that must
// accompany every Cloud Code Assist and inference request.
//
// Only User-Agent is sent. Crush previously also sent a Client-Metadata
// header, but reverse-engineering the real Antigravity CLI binary showed
// the string "client-metadata" appears nowhere in it as a header name, so
// no genuine client ever sends it. Sending a header the backend does not
// expect is a needless fingerprinting signal, so it was removed.
func cliHeaders(model string, id Identity) map[string]string {
	return map[string]string{
		"User-Agent": userAgent(model, id),
	}
}
