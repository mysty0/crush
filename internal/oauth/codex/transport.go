package codex

import "net/http"

// Inference header names and static values applied to every signed
// request.
const (
	headerAuthorization = "Authorization"
	headerAccountID     = "chatgpt-account-id"
	headerOpenAIBeta    = "OpenAI-Beta"
	headerOriginator    = "originator"

	openAIBetaResponses = "responses=experimental"
)

// AuthTransport signs outgoing Codex inference requests. On every request
// it sets the Bearer authorization from AccessToken, derives the
// chatgpt-account-id from the token's JWT, and sets the static
// OpenAI-Beta and originator headers.
//
// The coordinator rebuilds the provider with a fresh access token on
// refresh, so the transport simply carries the current access token
// rather than refreshing itself.
type AuthTransport struct {
	// Base is the underlying transport. When nil,
	// http.DefaultTransport is used.
	Base http.RoundTripper
	// AccessToken is the current Codex access token (a JWT).
	AccessToken string
}

// NewAuthTransport returns an AuthTransport that signs requests with the
// given access token, using base as the underlying transport (or
// http.DefaultTransport when base is nil).
func NewAuthTransport(base http.RoundTripper, accessToken string) *AuthTransport {
	return &AuthTransport{Base: base, AccessToken: accessToken}
}

// RoundTrip sets the Codex authentication headers and delegates to the
// base transport. It clones the request so the caller's headers are not
// mutated.
func (t *AuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	req = req.Clone(req.Context())
	req.Header.Set(headerAuthorization, "Bearer "+t.AccessToken)
	req.Header.Set(headerAccountID, DecodeAccountID(t.AccessToken))
req.Header.Set(headerOpenAIBeta, openAIBetaResponses)
req.Header.Set(headerOriginator, originator)
req.Header.Set("User-Agent", "codex_cli_rs/1.0.0 (Linux; x86_64)")

	return base.RoundTrip(req)
}
