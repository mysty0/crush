package geminicli

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

// fakeLanguageModel is a fantasy.LanguageModel double that returns a
// canned response/error from Generate and yields a canned stream part.
type fakeLanguageModel struct {
	resp *fantasy.Response
	err  error
}

func (f *fakeLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return f.resp, f.err
}

func (f *fakeLanguageModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		if f.err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: f.err})
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "ok"})
	}, nil
}

func (f *fakeLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, nil
}

func (f *fakeLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, nil
}

func (f *fakeLanguageModel) Provider() string { return "google" }
func (f *fakeLanguageModel) Model() string    { return "gemini-test" }

func rateLimitError(details []map[string]any) *fantasy.ProviderError {
	apiErr := genai.APIError{
		Code:    429,
		Message: "resource exhausted",
		Status:  "RESOURCE_EXHAUSTED",
		Details: details,
	}
	return &fantasy.ProviderError{
		Message:    apiErr.Message,
		StatusCode: 429,
		Cause:      apiErr,
	}
}

// streamError drains a wrapped stream and returns the first error part.
func streamError(t *testing.T, m fantasy.LanguageModel) error {
	t.Helper()
	stream, err := m.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			return part.Error
		}
	}
	return nil
}

func TestRateLimitAwareRetryDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		details []map[string]any
		want    string
	}{
		{
			name: "retry info detail",
			details: []map[string]any{
				{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "30s"},
			},
			want: "30",
		},
		{
			name: "fractional delay rounds up",
			details: []map[string]any{
				{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "1.500s"},
			},
			want: "2",
		},
		{
			name: "snake case key",
			details: []map[string]any{
				{"retry_delay": "12s"},
			},
			want: "12",
		},
		{
			name: "nested retry info",
			details: []map[string]any{
				{"retryInfo": map[string]any{"retryDelay": "45s"}},
			},
			want: "45",
		},
		{
			name: "scanned past unrelated details",
			details: []map[string]any{
				{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "violations": "many"},
				{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "7s"},
			},
			want: "7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &rateLimitAwareModel{LanguageModel: &fakeLanguageModel{err: rateLimitError(tt.details)}}

			_, err := m.Generate(t.Context(), fantasy.Call{})
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, tt.want, providerErr.ResponseHeaders["retry-after"])

			var streamProviderErr *fantasy.ProviderError
			require.ErrorAs(t, streamError(t, m), &streamProviderErr)
			require.Equal(t, tt.want, streamProviderErr.ResponseHeaders["retry-after"])
		})
	}
}

func TestRateLimitAwareNoParseableDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		details []map[string]any
	}{
		{name: "no details"},
		{
			name:    "unrelated details only",
			details: []map[string]any{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED"}},
		},
		{
			name:    "unparseable delay",
			details: []map[string]any{{"retryDelay": "soon"}},
		},
		{
			name:    "non-string delay",
			details: []map[string]any{{"retryDelay": 30}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			original := rateLimitError(tt.details)
			m := &rateLimitAwareModel{LanguageModel: &fakeLanguageModel{err: original}}

			_, err := m.Generate(t.Context(), fantasy.Call{})
			require.Same(t, original, err)
			require.Nil(t, original.ResponseHeaders)

			require.Same(t, original, streamError(t, m))
			require.Nil(t, original.ResponseHeaders)
		})
	}
}

func TestRateLimitAwareKeepsExistingRetryAfter(t *testing.T) {
	t.Parallel()

	original := rateLimitError([]map[string]any{{"retryDelay": "30s"}})
	original.ResponseHeaders = map[string]string{"retry-after": "5"}
	m := &rateLimitAwareModel{LanguageModel: &fakeLanguageModel{err: original}}

	_, err := m.Generate(t.Context(), fantasy.Call{})
	require.Same(t, original, err)
	require.Equal(t, "5", original.ResponseHeaders["retry-after"])
}

func TestRateLimitAwarePassesThroughNonRateLimitError(t *testing.T) {
	t.Parallel()

	original := &fantasy.ProviderError{Message: "boom", StatusCode: 500}
	m := &rateLimitAwareModel{LanguageModel: &fakeLanguageModel{err: original}}

	_, err := m.Generate(t.Context(), fantasy.Call{})
	require.Same(t, original, err)
	require.Same(t, original, streamError(t, m))

	plain := errors.New("not a provider error")
	m = &rateLimitAwareModel{LanguageModel: &fakeLanguageModel{err: plain}}
	_, err = m.Generate(t.Context(), fantasy.Call{})
	require.Same(t, plain, err)
}

func TestRateLimitAwarePassesThroughSuccess(t *testing.T) {
	t.Parallel()

	resp := &fantasy.Response{FinishReason: fantasy.FinishReasonStop}
	m := &rateLimitAwareModel{LanguageModel: &fakeLanguageModel{resp: resp}}

	got, err := m.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	require.Same(t, resp, got)

	stream, err := m.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeTextDelta, parts[0].Type)
	require.Equal(t, "ok", parts[0].Delta)
}

func TestRateLimitAwareStreamStopsOnYieldFalse(t *testing.T) {
	t.Parallel()

	m := &rateLimitAwareModel{LanguageModel: &fakeLanguageModel{resp: &fantasy.Response{}}}
	stream, err := m.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	count := 0
	for range stream {
		count++
		break
	}
	require.Equal(t, 1, count)
}

func TestRateLimitAwareProviderWrapsModel(t *testing.T) {
	t.Parallel()

	inner := &fakeLanguageModel{err: rateLimitError([]map[string]any{{"retryDelay": "20s"}})}
	p := WrapRateLimitAware(&fakeProvider{model: inner})

	m, err := p.LanguageModel(t.Context(), "gemini-test")
	require.NoError(t, err)
	require.IsType(t, &rateLimitAwareModel{}, m)

	_, err = m.Generate(t.Context(), fantasy.Call{})
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "20", providerErr.ResponseHeaders["retry-after"])
}

type fakeProvider struct {
	model fantasy.LanguageModel
}

func (f *fakeProvider) LanguageModel(context.Context, string) (fantasy.LanguageModel, error) {
	return f.model, nil
}

func (f *fakeProvider) Name() string { return "google" }
