package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyUnclassifiedStreamError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantReason string
		wantOK     bool
	}{
		{"nil error", nil, "", false},
		{
			"rate limit",
			errors.New(`received error while streaming: {"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`),
			"Rate limited", true,
		},
		{
			"overloaded",
			errors.New(`received error while streaming: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
			"Overloaded", true,
		},
		{
			"server error",
			errors.New(`received error while streaming: {"type":"error","error":{"type":"api_error","message":"Internal error"}}`),
			"Server error", true,
		},
		{
			"non-retryable type",
			errors.New(`received error while streaming: {"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`),
			"", false,
		},
		{
			"malformed JSON",
			errors.New(`received error while streaming: not json`),
			"", false,
		},
		{
			"unrelated error",
			errors.New("connection reset by peer"),
			"", false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, ok := classifyUnclassifiedStreamError(tt.err)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestRetryStreamErrorSucceedsImmediately(t *testing.T) {
	t.Parallel()

	calls := 0
	err := retryStreamError(context.Background(), func() error {
		calls++
		return nil
	}, 5, time.Millisecond, 2.0)

	require.NoError(t, err)
	require.Equal(t, 1, calls, "a successful first call must not retry")
}

func TestRetryStreamErrorNonRetryableStopsImmediately(t *testing.T) {
	t.Parallel()

	calls := 0
	wantErr := errors.New("boom")
	err := retryStreamError(context.Background(), func() error {
		calls++
		return wantErr
	}, 5, time.Millisecond, 2.0)

	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 1, calls, "a non-retryable error must not be retried")
}

func TestRetryStreamErrorRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	retryable := errors.New(`received error while streaming: {"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`)
	calls := 0
	err := retryStreamError(context.Background(), func() error {
		calls++
		if calls < 3 {
			return retryable
		}
		return nil
	}, 5, time.Millisecond, 2.0)

	require.NoError(t, err)
	require.Equal(t, 3, calls, "must retry until the call succeeds")
}

func TestRetryStreamErrorGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()

	retryable := errors.New(`received error while streaming: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	calls := 0
	maxRetries := 3
	err := retryStreamError(context.Background(), func() error {
		calls++
		return retryable
	}, maxRetries, time.Millisecond, 2.0)

	require.ErrorIs(t, err, retryable)
	require.Equal(t, maxRetries+1, calls, "must attempt the initial call plus maxRetries retries")
}

func TestRetryStreamErrorRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	retryable := errors.New(`received error while streaming: {"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryStreamError(ctx, func() error {
		calls++
		if calls == 1 {
			cancel()
		}
		return retryable
	}, 5, time.Hour, 2.0) // a long delay proves cancellation, not a timeout, unblocks the wait

	require.ErrorIs(t, err, retryable)
	require.Equal(t, 1, calls, "cancellation during the backoff wait must stop further retries")
}

func TestRetryStreamErrorEmptyErrorMessageNeverPanics(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		_, _ = classifyUnclassifiedStreamError(fmt.Errorf(""))
	})
}
