package codex

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubRoundTripper captures the request it receives and returns a canned
// response.
type stubRoundTripper struct {
	req *http.Request
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.req = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestAuthTransport_SetsHeaders(t *testing.T) {
	t.Parallel()

	access := makeJWT(t, map[string]any{
		accountClaim: map[string]any{"chatgpt_account_id": "acct-777"},
	})

	stub := &stubRoundTripper{}
	rt := NewAuthTransport(stub, access)

	req, err := http.NewRequest(http.MethodPost, BaseURL+"/responses", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, "Bearer "+access, stub.req.Header.Get("Authorization"))
	require.Equal(t, "acct-777", stub.req.Header.Get("chatgpt-account-id"))
	require.Equal(t, "responses=experimental", stub.req.Header.Get("OpenAI-Beta"))
	require.Equal(t, "pi", stub.req.Header.Get("originator"))

	// The original request must not be mutated.
	require.Empty(t, req.Header.Get("Authorization"))
}

func TestAuthTransport_NilBaseUsesDefault(t *testing.T) {
	t.Parallel()

	rt := &AuthTransport{AccessToken: "tok"}
	require.NotNil(t, rt)
	require.Nil(t, rt.Base)
}
