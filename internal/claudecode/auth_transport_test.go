package claudecode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureTransport records the request it was asked to send, standing in
// for the real network round trip.
type captureTransport struct {
	got *http.Request
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = req.Clone(req.Context())
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	return rec.Result(), nil
}

// TestAuthTransportReplacesPlaceholderAPIKey guards the subscription auth
// path: the Anthropic SDK refuses to send a request unless it can see a
// credential of its own, and it decides that before any transport runs.
// Crush therefore hands it PlaceholderAPIKey, which this transport must
// swap for the real bearer token so the placeholder is never transmitted.
func TestAuthTransportReplacesPlaceholderAPIKey(t *testing.T) {
	t.Parallel()

	cap := &captureTransport{}
	tr := &AuthTransport{
		Base: cap,
		Source: NewTokenSource(TokenFunc(func(context.Context) (string, error) {
			return "real-token", nil
		})),
	}

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("X-Api-Key", PlaceholderAPIKey)

	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, "Bearer real-token", cap.got.Header.Get("Authorization"))
	assert.Empty(t, cap.got.Header.Get("X-Api-Key"), "placeholder key must never be transmitted")
	assert.Contains(t, cap.got.Header.Get("anthropic-beta"), OAuthBeta)
}

// TestAuthTransportDropsPlaceholderWhenTokenFails ensures a failed token
// refresh degrades to an unauthenticated request rather than sending the
// placeholder as if it were a real key, which would surface as a
// misleading authentication failure from the API.
func TestAuthTransportDropsPlaceholderWhenTokenFails(t *testing.T) {
	t.Parallel()

	cap := &captureTransport{}
	tr := &AuthTransport{
		Base: cap,
		Source: NewTokenSource(TokenFunc(func(context.Context) (string, error) {
			return "", errors.New("token refresh failed")
		})),
	}

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("X-Api-Key", PlaceholderAPIKey)

	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Empty(t, cap.got.Header.Get("X-Api-Key"), "placeholder key must never be transmitted")
	assert.Empty(t, cap.got.Header.Get("Authorization"))
}
