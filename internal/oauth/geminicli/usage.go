package geminicli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// UsageBucket is a single named quota window returned by the Cloud Code
// Assist retrieveUserQuotaSummary RPC, e.g. "Gemini 2.5 Pro" over a "24h"
// window.
type UsageBucket struct {
	// Label is the bucket's (or, if the bucket has none, its group's)
	// display name, e.g. "Gemini 2.5 Pro".
	Label string
	// Window is the human-readable window the bucket covers, e.g. "24h".
	// It is empty when the server does not report one.
	Window string
	// RemainingFraction is the fraction of quota still available for
	// this bucket, in the range 0..1. It is -1 when the server reported
	// a raw remaining amount instead of a fraction, or reported neither.
	RemainingFraction float64
	// ResetsAt is when this bucket's quota resets. It is the zero time
	// when unknown.
	ResetsAt time.Time
	// Disabled reports whether this bucket does not apply to the
	// account and should not be displayed.
	Disabled bool
}

// Usage is a Gemini CLI (Cloud Code Assist) subscription quota snapshot.
type Usage struct {
	// Tier is the Cloud Code Assist tier id, e.g. "free-tier". It is empty
	// when the tier could not be determined.
	Tier string
	// Buckets are the account's quota windows, one per model or feature
	// group, as returned by retrieveUserQuotaSummary.
	Buckets []UsageBucket
}

// quotaSummaryResponse is the parsed body of a retrieveUserQuotaSummary
// call. Field names and the overall message shape (groups of buckets,
// each with a display name, window, and either a remaining fraction or
// a raw remaining amount) were recovered by disassembling the real
// Antigravity CLI binary's compiled protobuf descriptor for
// google.internal.cloud.code.v1internal.RetrieveUserQuotaSummaryResponse;
// see docs/antigravity-cli-oauth-findings.md.
type quotaSummaryResponse struct {
	Groups []struct {
		DisplayName string `json:"displayName"`
		Buckets     []struct {
			DisplayName       string   `json:"displayName"`
			Window            string   `json:"window"`
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
			Disabled          bool     `json:"disabled"`
		} `json:"buckets"`
	} `json:"groups"`
}

// FetchUsage retrieves the Cloud Code Assist quota for the given project.
//
// It first performs a best-effort loadCodeAssist call to learn the current
// tier; failures there are logged and ignored so the tier simply stays
// empty. It then calls retrieveUserQuotaSummary to derive the account's
// quota buckets, returning an error only when that call fails.
func FetchUsage(ctx context.Context, accessToken, projectID string, id Identity) (*Usage, error) {
	usage := &Usage{}

	// Best-effort tier lookup. Any failure is swallowed because the tier
	// is optional context for the quota reading.
	if tier, err := fetchTier(ctx, accessToken, projectID, id); err != nil {
		slog.Debug("geminicli: loadCodeAssist for usage failed",
			"error", err)
	} else {
		usage.Tier = tier
	}

	buckets, err := fetchQuotaBuckets(ctx, accessToken, projectID, id)
	if err != nil {
		return nil, err
	}
	usage.Buckets = buckets
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

// fetchQuotaBuckets performs a retrieveUserQuotaSummary call and flattens
// every group's buckets into a single list. Disabled buckets are kept
// (callers decide whether to display them) since the caller may still
// want to know they exist.
//
// The request's project field is named "project", not
// "cloudaicompanionProject" like loadCodeAssist/onboardUser -- confirmed
// by disassembling RetrieveUserQuotaSummaryRequest's compiled protobuf
// descriptor (see docs/antigravity-cli-oauth-findings.md). Sending the
// wrong field name previously left the server-required project
// unset, which is the root cause of a prior "usage" failure for Gemini
// CLI/Antigravity accounts.
func fetchQuotaBuckets(ctx context.Context, accessToken, projectID string, id Identity) ([]UsageBucket, error) {
	body := map[string]any{
		"project": projectID,
	}
	respBody, status, err := codeAssistPost(ctx, accessToken, "retrieveUserQuotaSummary", body, id)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("geminicli: retrieveUserQuotaSummary failed: %s - %s",
			http.StatusText(status), strings.TrimSpace(string(respBody)))
	}

	var parsed quotaSummaryResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("geminicli: parse retrieveUserQuotaSummary response: %w", err)
	}

	var buckets []UsageBucket
	for _, g := range parsed.Groups {
		for _, b := range g.Buckets {
			label := b.DisplayName
			if label == "" {
				label = g.DisplayName
			}
			frac := -1.0
			if b.RemainingFraction != nil {
				frac = clampFraction(*b.RemainingFraction)
			}
			var resetsAt time.Time
			if b.ResetTime != "" {
				if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
					resetsAt = t
				}
			}
			buckets = append(buckets, UsageBucket{
				Label:             label,
				Window:            b.Window,
				RemainingFraction: frac,
				ResetsAt:          resetsAt,
				Disabled:          b.Disabled,
			})
		}
	}
	return buckets, nil
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
