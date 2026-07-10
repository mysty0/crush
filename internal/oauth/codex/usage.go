package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// UsageWindow is one rate-limit window of a Codex subscription plan.
type UsageWindow struct {
	// UsedPercent is the fraction of the window consumed, from 0 to 100.
	// It is -1 when unknown.
	UsedPercent float64
	// WindowSeconds is the length of the window in seconds. It is 0 when
	// unknown.
	WindowSeconds int64
	// ResetsAt is when the window resets. It is the zero time when
	// unknown.
	ResetsAt time.Time
}

// Usage is a Codex subscription plan-usage snapshot.
type Usage struct {
	// PlanType is the subscription plan name (for example "plus" or
	// "pro").
	PlanType string
	// Allowed reports whether requests are currently permitted.
	Allowed bool
	// LimitReached reports whether a rate limit has been hit.
	LimitReached bool
	// Primary is the primary rate-limit window, or nil when absent.
	Primary *UsageWindow
	// Secondary is the secondary rate-limit window, or nil when absent.
	Secondary *UsageWindow
}

// usageResponse mirrors the /wham/usage payload. Every field is optional.
type usageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed         *bool              `json:"allowed"`
		LimitReached    *bool              `json:"limit_reached"`
		PrimaryWindow   *usageWindowResult `json:"primary_window"`
		SecondaryWindow *usageWindowResult `json:"secondary_window"`
	} `json:"rate_limit"`
}

// usageWindowResult is the raw JSON shape of a single rate-limit window.
type usageWindowResult struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *float64 `json:"reset_at"`
}

// FetchUsage retrieves the Codex subscription plan-usage snapshot from
// the native ChatGPT account API (/wham/usage). The account id used for
// the request header is derived from the access token via
// DecodeAccountID. It returns an error on transport failure or a non-200
// response. When the payload carries no rate-limit block, it returns a
// snapshot with only the plan type populated.
func FetchUsage(ctx context.Context, accessToken string) (*Usage, error) {
	url := accountAPIBaseURL + "/wham/usage"
	req, err := newAccountAPIRequest(ctx, url, accessToken)
	if err != nil {
		return nil, err
	}

	resp, err := accountHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex: usage request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codex: reading usage response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex: usage request: %s - %s",
			http.StatusText(resp.StatusCode), strings.TrimSpace(string(body)))
	}

	var parsed usageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("codex: parsing usage response: %w", err)
	}

	usage := &Usage{PlanType: parsed.PlanType}
	if parsed.RateLimit == nil {
		// No rate-limit block is not an error; return the plan type with
		// nil windows.
		return usage, nil
	}

	if parsed.RateLimit.Allowed != nil {
		usage.Allowed = *parsed.RateLimit.Allowed
	}
	if parsed.RateLimit.LimitReached != nil {
		usage.LimitReached = *parsed.RateLimit.LimitReached
	}
	usage.Primary = parseUsageWindow(parsed.RateLimit.PrimaryWindow)
	usage.Secondary = parseUsageWindow(parsed.RateLimit.SecondaryWindow)

	return usage, nil
}

// parseUsageWindow converts a raw rate-limit window into a UsageWindow,
// applying the reset-time and used-percent rules. It returns nil when the
// input window is absent.
func parseUsageWindow(w *usageWindowResult) *UsageWindow {
	if w == nil {
		return nil
	}

	out := &UsageWindow{UsedPercent: -1}

	if w.LimitWindowSeconds != nil {
		out.WindowSeconds = *w.LimitWindowSeconds
	}

	if w.UsedPercent != nil {
		pct := *w.UsedPercent
		switch {
		case pct < 0:
			pct = 0
		case pct > 100:
			pct = 100
		}
		out.UsedPercent = pct
	}

	out.ResetsAt = resolveResetTime(w)

	return out
}

// resolveResetTime derives a window's reset time. It prefers an absolute
// reset_at epoch (interpreted as milliseconds when larger than 1e12,
// otherwise seconds), then falls back to reset_after_seconds relative to
// now. It returns the zero time when neither is present.
func resolveResetTime(w *usageWindowResult) time.Time {
	if w.ResetAt != nil {
		epoch := *w.ResetAt
		if epoch > 1e12 {
			// Milliseconds since the Unix epoch.
			ms := int64(epoch)
			return time.UnixMilli(ms)
		}
		// Seconds since the Unix epoch.
		return time.Unix(int64(epoch), 0)
	}
	if w.ResetAfterSeconds != nil {
		return time.Now().Add(time.Duration(*w.ResetAfterSeconds) * time.Second)
	}
	return time.Time{}
}
