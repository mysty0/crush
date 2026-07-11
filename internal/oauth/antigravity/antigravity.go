// Package antigravity implements OAuth login and token refresh for Google
// Antigravity CLI subscriptions.
//
// EXPERIMENTAL: Google has not published Antigravity CLI's OAuth protocol.
// Every constant below was recovered by extracting strings from the
// official antigravity.google/cli/install.sh binary (agy v1.1.1); none of
// it has been exercised against a live Google endpoint. See
// docs/antigravity-cli-oauth-findings.md for the full methodology and
// confidence notes before relying on this package.
//
// Antigravity shares its inference backend (cloudcode-pa.googleapis.com)
// and onboarding/quota RPCs with Gemini CLI (Cloud Code Assist), so this
// package only implements the OAuth login/refresh that is unique to
// Antigravity's client registration. Project discovery, the inference
// wire transport, and usage reporting are provided by the sibling
// geminicli package and reused as-is by callers.
package antigravity

import "github.com/charmbracelet/crush/internal/oauth/geminicli"

// ProviderID is the reserved Crush provider id that activates native
// Google Antigravity subscription handling.
const ProviderID = "google-antigravity"

// BaseURL is the Cloud Code Assist API base URL Antigravity shares with
// Gemini CLI; see geminicli.BaseURL.
const BaseURL = geminicli.BaseURL

const (
	// clientID is Antigravity CLI's public OAuth client id for the
	// "consumer" auth method — CONFIRMED by disassembling the real agy
	// v1.1.1 binary's google3/third_party/jetski/cli/backend/auth/auth
	// package (see docs/antigravity-cli-oauth-findings.md, "Live
	// verification, round 4"). The binary's chainedAuthOrDefault and
	// getOauthParams functions define exactly two client
	// registrations, selected by comparing an authMethod string against
	// the literal "gcp" (falling through to this one, the "consumer"
	// pairing, otherwise); the exact string length (73 bytes) and
	// content were verified byte-for-byte against the disassembled
	// immediate operand, not guessed. It is not a secret; Google's
	// installed-app OAuth model does not treat these as confidential.
	clientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"

	// clientSecret is the "consumer" method's paired client "secret",
	// confirmed by the same disassembly (35-byte length verified against
	// the immediate operand). Installed applications cannot keep a real
	// secret, so Google treats this as a public identifier rather than a
	// credential.
	clientSecret = "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"

	// callbackPort is the preferred loopback port for the OAuth redirect
	// in the browser flow. Distinct from geminicli's 8085 so both can be
	// configured without a port clash. Confirmed live: Google's authorize
	// endpoint accepts an arbitrary 127.0.0.1 loopback redirect_uri for
	// this client family without requiring
	// http://antigravity.google/oauth-callback, i.e. it is registered as
	// a "Desktop app" / installed-app OAuth client per RFC 8252.
	callbackPort = 8086
	// callbackPath is the OAuth redirect path used for the loopback flow.
	callbackPath = "/oauth2callback"
	// callbackHostname is the loopback host used in the redirect URI.
	callbackHostname = "127.0.0.1"
)

// scopes are the OAuth scopes requested for an Antigravity login. This is
// intentionally narrower than every scope observed in the binary (which
// also requests several Drive scopes for Antigravity's Docs/Drive
// integration); Crush only needs inference access.
var scopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

// Endpoint URLs are package-level variables so tests can point them at
// httptest servers.
var (
	// authURL is Google's OAuth 2.0 authorization endpoint.
	authURL = "https://accounts.google.com/o/oauth2/auth"
	// tokenURL is Google's OAuth 2.0 token endpoint.
	tokenURL = "https://oauth2.googleapis.com/token"
	// deviceCodeURL is Google's RFC 8628 device authorization endpoint.
	deviceCodeURL = "https://oauth2.googleapis.com/device/code"
)

// cliVersion is the Antigravity CLI version reported in the User-Agent,
// matching the version observed in the published install manifest at the
// time this package was written.
const cliVersion = "1.1.1"

// Identity is the client identity Crush reports for Antigravity requests.
//
// pluginType=CLOUD_CODE, ideType=ANTIGRAVITY are grounded in the real
// protobuf schema (extracted from the ClientMetadata descriptor; see
// docs/antigravity-cli-oauth-findings.md) and confirmed NOT to trigger a
// validation error from loadCodeAssist. They turned out not to be the
// actual cause of the tier problem that motivated recovering them,
// though — that was traced to using the wrong OAuth client/secret pair
// (see the clientID/clientSecret comments above), not the client
// identity metadata.
var Identity = geminicli.Identity{
	Product:    "Antigravity",
	Version:    cliVersion,
	PluginType: "CLOUD_CODE",
	IDEType:    "ANTIGRAVITY",
}
