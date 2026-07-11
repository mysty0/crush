package geminicli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Usage is a Gemini CLI (Cloud Code Assist) subscription quota snapshot.
type Usage struct {
	// Tier is the Cloud Code Assist tier id, e.g. "free-tier". It is empty
	// when the tier could not be determined.
	Tier string
	// RemainingFraction is the fraction of quota still available, in the
	// range 0..1. It is -1 when the remaining quota is unknown.
	RemainingFraction float64
}

// quotaResponse is the defensive shape of a retrieveUserQuota response. The
// Cloud Code Assist backend reports remaining quota under one of several
// field layouts, so every field is optional.
type quotaResponse struct {
	RemainingFraction *float64 `json:"remainingFraction"`
	Quota             *struct {
		RemainingFraction *float64 `json:"remainingFraction"`
	} `json:"quota"`
	Remaining *float64 `json:"remaining"`
	Limit     *float64 `json:"limit"`
}

// FetchUsage retrieves the Cloud Code Assist quota for the given project.
//
// It first performs a best-effort loadCodeAssist call to learn the current
// tier; failures there are logged and ignored so the tier simply stays
// empty. It then calls retrieveUserQuota to derive the remaining quota
// fraction, returning an error only when that call fails.
func FetchUsage(ctx context.Context, accessToken, projectID string, id Identity) (*Usage, error) {
	usage := &Usage{RemainingFraction: -1}

	// Best-effort tier lookup. Any failure is swallowed because the tier
	// is optional context for the quota reading.
	if tier, err := fetchTier(ctx, accessToken, projectID, id); err != nil {
		slog.Debug("geminicli: loadCodeAssist for usage failed",
			"error", err)
	} else {
		usage.Tier = tier
	}

	frac, err := fetchRemainingFraction(ctx, accessToken, projectID, id)
	if err != nil {
		return nil, err
	}
	usage.RemainingFraction = frac
	return usage, nil
}

// fetchTier performs a loadCodeAssist call and returns the reported current
// tier id, if any.
func fetchTier(ctx context.Context, accessToken, projectID string, id Identity) (string, error) {
	body := map[string]any{
		"cloudaicompanionProject": projectID,
		"metadata":                baseMetadata(projectID, id),
	}
	respBody, status, err := codeAssistPost(ctx, accessToken, "loadCodeAssist", body, id)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("geminicli: loadCodeAssist failed: %s - %s",
			http.StatusText(status), strings.TrimSpace(string(respBody)))
	}
	var out struct {
		CurrentTier *struct {
			ID string `json:"id"`
		} `json:"currentTier"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("geminicli: parse loadCodeAssist response: %w", err)
	}
	if out.CurrentTier != nil {
		return out.CurrentTier.ID, nil
	}
	return "", nil
}

// fetchRemainingFraction performs a retrieveUserQuota call and derives the
// remaining quota fraction, clamped to the range 0..1. It returns -1 when
// the response carries no usable field.
func fetchRemainingFraction(ctx context.Context, accessToken, projectID string, id Identity) (float64, error) {
	body := map[string]any{
		"cloudaicompanionProject": projectID,
	}
	respBody, status, err := codeAssistPost(ctx, accessToken, "retrieveUserQuota", body, id)
	if err != nil {
		return -1, err
	}
	if status != http.StatusOK {
		return -1, fmt.Errorf("geminicli: retrieveUserQuota failed: %s - %s",
			http.StatusText(status), strings.TrimSpace(string(respBody)))
	}

	var q quotaResponse
	if err := json.Unmarshal(respBody, &q); err != nil {
		return -1, fmt.Errorf("geminicli: parse retrieveUserQuota response: %w", err)
	}

	switch {
	case q.RemainingFraction != nil:
		return clampFraction(*q.RemainingFraction), nil
	case q.Quota != nil && q.Quota.RemainingFraction != nil:
		return clampFraction(*q.Quota.RemainingFraction), nil
	case q.Remaining != nil && q.Limit != nil && *q.Limit != 0:
		return clampFraction(*q.Remaining / *q.Limit), nil
	default:
		return -1, nil
	}
}

// clampFraction constrains a quota fraction to the closed range 0..1.
func clampFraction(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
