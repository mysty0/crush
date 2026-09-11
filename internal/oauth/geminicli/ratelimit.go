package geminicli

import (
	"context"
	"errors"
	"maps"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"google.golang.org/genai"
)

// WrapRateLimitAware decorates a provider so 429 responses carry the wait
// time the backend actually asked for.
//
// fantasy's retry middleware honors a server-supplied delay, but only reads
// it from ProviderError.ResponseHeaders ("retry-after-ms" / "retry-after").
// The google provider builds its ProviderError from a genai.APIError and
// never populates those headers, so every rate-limit response falls back to
// blind exponential backoff and ignores Google's own retry hint, which
// arrives as a google.rpc.RetryInfo entry in the error details instead of an
// HTTP header. This wrapper copies that hint into the header the retry
// middleware reads.
func WrapRateLimitAware(p fantasy.Provider) fantasy.Provider {
	return &rateLimitAwareProvider{Provider: p}
}

type rateLimitAwareProvider struct {
	fantasy.Provider
}

func (p *rateLimitAwareProvider) LanguageModel(ctx context.Context, modelID string) (fantasy.LanguageModel, error) {
	m, err := p.Provider.LanguageModel(ctx, modelID)
	if err != nil {
		return nil, err
	}
	return &rateLimitAwareModel{LanguageModel: m}, nil
}

type rateLimitAwareModel struct {
	fantasy.LanguageModel
}

func (m *rateLimitAwareModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	resp, err := m.LanguageModel.Generate(ctx, call)
	if err != nil {
		return resp, enrichRateLimitError(err)
	}
	return resp, nil
}

func (m *rateLimitAwareModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, enrichRateLimitError(err)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range stream {
			// Stream failures are reported as an error part rather than
			// as the outer error, so they need the same enrichment.
			if part.Type == fantasy.StreamPartTypeError && part.Error != nil {
				part.Error = enrichRateLimitError(part.Error)
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

// enrichRateLimitError returns a copy of a rate-limit error with a
// "retry-after" response header derived from Google's structured error
// details. Any error that isn't a rate limit, or that carries no parseable
// retry hint, is returned untouched.
func enrichRateLimitError(err error) error {
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return err
	}
	if providerErr.StatusCode != http.StatusTooManyRequests {
		return err
	}
	if hasRetryAfterHeader(providerErr.ResponseHeaders) {
		return err
	}
	var apiErr genai.APIError
	if !errors.As(providerErr.Cause, &apiErr) {
		return err
	}
	delay, ok := retryDelayFromDetails(apiErr.Details, 0)
	if !ok {
		return err
	}

	// Copy rather than mutate: the original error may be observed
	// elsewhere in the call path.
	enriched := *providerErr
	enriched.ResponseHeaders = maps.Clone(providerErr.ResponseHeaders)
	if enriched.ResponseHeaders == nil {
		enriched.ResponseHeaders = make(map[string]string, 1)
	}
	// Round up so a fractional delay never retries early.
	seconds := int64(math.Ceil(delay.Seconds()))
	enriched.ResponseHeaders["retry-after"] = strconv.FormatInt(seconds, 10)
	return &enriched
}

// hasRetryAfterHeader reports whether a retry delay was already provided
// upstream, in which case it must not be overridden.
func hasRetryAfterHeader(headers map[string]string) bool {
	for k := range headers {
		if strings.EqualFold(k, "retry-after") || strings.EqualFold(k, "retry-after-ms") {
			return true
		}
	}
	return false
}

// maxRetryDelayDepth bounds the search through nested error details.
const maxRetryDelayDepth = 3

// retryDelayFromDetails looks for a google.rpc.RetryInfo-style retry delay
// in the generically unmarshaled error details. Only fields named
// "retryDelay" (in any case/underscore spelling) holding a duration-shaped
// string are considered, so unrelated detail data is never misread.
func retryDelayFromDetails(details []map[string]any, depth int) (time.Duration, bool) {
	if depth > maxRetryDelayDepth {
		return 0, false
	}
	for _, detail := range details {
		for k, v := range detail {
			if isRetryDelayKey(k) {
				if delay, ok := parseRetryDelay(v); ok {
					return delay, true
				}
				continue
			}
			nested, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if delay, ok := retryDelayFromDetails([]map[string]any{nested}, depth+1); ok {
				return delay, true
			}
		}
	}
	return 0, false
}

func isRetryDelayKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
	return normalized == "retrydelay"
}

// parseRetryDelay parses a protobuf Duration serialized as a string, e.g.
// "30s" or "1.500s", which time.ParseDuration handles natively.
func parseRetryDelay(value any) (time.Duration, bool) {
	s, ok := value.(string)
	if !ok {
		return 0, false
	}
	delay, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || delay <= 0 {
		return 0, false
	}
	return delay, true
}
