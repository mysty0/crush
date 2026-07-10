package callback

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServerCapturesCode(t *testing.T) {
	t.Parallel()

	srv, err := Start(Config{Path: "/auth/callback", AllowPortFallback: true})
	require.NoError(t, err)
	defer srv.Close()

	require.True(t, strings.HasPrefix(srv.RedirectURI(), "http://localhost:"))
	require.Contains(t, srv.RedirectURI(), "/auth/callback")

	// Simulate the provider redirecting the browser back with a code.
	go func() {
		resp, err := http.Get(srv.RedirectURI() + "?code=the-code&state=xyz")
		if err == nil {
			resp.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := srv.Wait(ctx)
	require.NoError(t, err)
	require.Equal(t, "the-code", res.Code)
	require.Equal(t, "xyz", res.State)
}

func TestServerReportsProviderError(t *testing.T) {
	t.Parallel()

	srv, err := Start(Config{Path: "/cb", AllowPortFallback: true})
	require.NoError(t, err)
	defer srv.Close()

	go func() {
		resp, err := http.Get(srv.RedirectURI() + "?error=access_denied&error_description=nope")
		if err == nil {
			resp.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = srv.Wait(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nope")
}

func TestServerWaitContextCancelled(t *testing.T) {
	t.Parallel()

	srv, err := Start(Config{Path: "/cb", AllowPortFallback: true})
	require.NoError(t, err)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = srv.Wait(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
