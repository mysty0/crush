package agent

import (
	"context"
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestRetryReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *fantasy.ProviderError
		want string
	}{
		{"nil", nil, "Connection issue"},
		{"rate limit", &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}, "Rate limited"},
		{"server error", &fantasy.ProviderError{StatusCode: http.StatusBadGateway}, "Server error"},
		{"title fallback", &fantasy.ProviderError{StatusCode: http.StatusBadRequest, Title: "overloaded"}, "Overloaded"},
		{"generic", &fantasy.ProviderError{StatusCode: http.StatusBadRequest}, "Provider error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, retryReason(tt.err))
		})
	}
}

func TestRetryProgressBrokerRoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub := SubscribeRetryProgress(ctx)

	want := RetryProgressEvent{
		SessionID:  "sess-1",
		MessageID:  "msg-1",
		Attempt:    3,
		MaxRetries: 5,
		RetryAt:    time.Now().Add(10 * time.Second),
		Reason:     "Rate limited",
	}
	PublishRetryProgress(want)

	select {
	case ev := <-sub:
		require.Equal(t, pubsub.UpdatedEvent, ev.Type)
		require.Equal(t, want, ev.Payload)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for retry progress event")
	}
}
