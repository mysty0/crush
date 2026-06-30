package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RateLimit describes a single rate-limit window returned by the Claude
// Code subscription usage endpoint. Utilization is a percentage from 0 to
// 100; ResetsAt is an ISO 8601 timestamp. Both are nil when the window is
// not applicable to the account.
type RateLimit struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// ExtraUsage describes pay-as-you-go credit usage beyond the subscription
// rate limits.
type ExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

// Utilization is the subscription plan usage reported by Anthropic for a
// Claude Code (Pro/Max) account. Each field is omitted by the API when it
// does not apply to the account's plan.
type Utilization struct {
	FiveHour          *RateLimit  `json:"five_hour"`
	SevenDay          *RateLimit  `json:"seven_day"`
	SevenDayOAuthApps *RateLimit  `json:"seven_day_oauth_apps"`
	SevenDayOpus      *RateLimit  `json:"seven_day_opus"`
	SevenDaySonnet    *RateLimit  `json:"seven_day_sonnet"`
	ExtraUsage        *ExtraUsage `json:"extra_usage"`
}

// Usage queries the subscription plan usage endpoint and returns the
// account's current rate-limit utilization. It requires a valid Claude
// Code OAuth token; it does not work with a plain Anthropic API key.
func (s *Source) Usage(ctx context.Context) (*Utilization, error) {
	token, err := s.Token(ctx)
	if err != nil {
		return nil, err
	}
	base := s.baseURL
	if base == "" {
		base = BaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/oauth/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", OAuthBeta)
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claudecode: usage request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("claudecode: usage: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var u Utilization
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("claudecode: decode usage: %w", err)
	}
	return &u, nil
}
