package geminicli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// pollInterval is how long to wait between long-running operation polls.
// It is a variable so tests can shorten it.
var pollInterval = 5 * time.Second

// cloudMetadata is the client metadata block sent on Cloud Code Assist
// onboarding requests.
type cloudMetadata struct {
	IDEType     string `json:"ideType"`
	Platform    string `json:"platform"`
	PluginType  string `json:"pluginType"`
	DuetProject string `json:"duetProject,omitempty"`
}

// baseMetadata returns the standard metadata block, optionally tagged with
// the caller's Cloud project id.
func baseMetadata(projectID string) cloudMetadata {
	return cloudMetadata{
		IDEType:     "IDE_UNSPECIFIED",
		Platform:    "PLATFORM_UNSPECIFIED",
		PluginType:  "GEMINI",
		DuetProject: projectID,
	}
}

// envProjectID reads the Cloud project id from the environment, checking
// both accepted variable names.
func envProjectID() string {
	if p := os.Getenv("GOOGLE_CLOUD_PROJECT"); p != "" {
		return p
	}
	return os.Getenv("GOOGLE_CLOUD_PROJECT_ID")
}

// DiscoverProject performs the Cloud Code Assist onboarding handshake and
// returns the Cloud project id that must be attached to every inference
// request. The access token must already be valid.
func DiscoverProject(ctx context.Context, accessToken string) (string, error) {
	envProject := envProjectID()

	load, err := loadCodeAssist(ctx, accessToken, envProject)
	if err != nil {
		return "", err
	}

	// When a current tier is already assigned the account is fully
	// onboarded; use the server-provided project, falling back to the
	// environment.
	if load.CurrentTier != nil {
		if load.CloudaicompanionProject != "" {
			return load.CloudaicompanionProject, nil
		}
		if envProject != "" {
			return envProject, nil
		}
		return "", fmt.Errorf("geminicli: no Cloud project available; set GOOGLE_CLOUD_PROJECT")
	}

	// Otherwise select a default tier and onboard the user.
	tierID := freeTier
	for _, t := range load.AllowedTiers {
		if t.IsDefault {
			if t.ID != "" {
				tierID = t.ID
			}
			break
		}
	}
	if tierID != freeTier && envProject == "" {
		return "", fmt.Errorf("geminicli: tier %q requires a Cloud project; set GOOGLE_CLOUD_PROJECT", tierID)
	}

	return onboardUser(ctx, accessToken, tierID, envProject)
}

// loadCodeAssistResponse is the parsed body of a loadCodeAssist call.
type loadCodeAssistResponse struct {
	CloudaicompanionProject string `json:"cloudaicompanionProject"`
	CurrentTier             *tier  `json:"currentTier"`
	AllowedTiers            []tier `json:"allowedTiers"`
}

// tier describes a Cloud Code Assist subscription tier.
type tier struct {
	ID        string `json:"id"`
	IsDefault bool   `json:"isDefault"`
}

// loadCodeAssist calls loadCodeAssist to learn the user's current tier and
// project. VPC-SC users whose request is blocked by a security policy are
// treated as already on the standard tier.
func loadCodeAssist(ctx context.Context, accessToken, envProject string) (*loadCodeAssistResponse, error) {
	body := map[string]any{
		"cloudaicompanionProject": envProject,
		"metadata":                baseMetadata(envProject),
	}
	respBody, status, err := codeAssistPost(ctx, accessToken, "loadCodeAssist", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if isSecurityPolicyViolation(respBody) {
			// VPC-SC restricted user: proceed as standard tier.
			return &loadCodeAssistResponse{CurrentTier: &tier{ID: standardTier}}, nil
		}
		return nil, fmt.Errorf("geminicli: loadCodeAssist failed: %s - %s", http.StatusText(status), string(respBody))
	}

	var out loadCodeAssistResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("geminicli: parse loadCodeAssist response: %w", err)
	}
	return &out, nil
}

// isSecurityPolicyViolation reports whether an error body carries a
// SECURITY_POLICY_VIOLATED reason, indicating a VPC-SC-restricted user.
func isSecurityPolicyViolation(body []byte) bool {
	var wrap struct {
		Error struct {
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return false
	}
	for _, d := range wrap.Error.Details {
		if d.Reason == "SECURITY_POLICY_VIOLATED" {
			return true
		}
	}
	return false
}

// longRunningOperation is the shape of an onboardUser (or poll) response.
type longRunningOperation struct {
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	Response struct {
		CloudaicompanionProject struct {
			ID string `json:"id"`
		} `json:"cloudaicompanionProject"`
	} `json:"response"`
}

// onboardUser onboards the account to the given tier and returns the
// resulting Cloud project id, polling the long-running operation until it
// completes.
func onboardUser(ctx context.Context, accessToken, tierID, envProject string) (string, error) {
	body := map[string]any{
		"tierId":   tierID,
		"metadata": baseMetadata(""),
	}
	if tierID != freeTier && envProject != "" {
		body["cloudaicompanionProject"] = envProject
		body["metadata"] = baseMetadata(envProject)
	}

	respBody, status, err := codeAssistPost(ctx, accessToken, "onboardUser", body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("geminicli: onboardUser failed: %s - %s", http.StatusText(status), string(respBody))
	}

	var op longRunningOperation
	if err := json.Unmarshal(respBody, &op); err != nil {
		return "", fmt.Errorf("geminicli: parse onboardUser response: %w", err)
	}

	for !op.Done && op.Name != "" {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
		op, err = pollOperation(ctx, accessToken, op.Name)
		if err != nil {
			return "", err
		}
	}

	if id := op.Response.CloudaicompanionProject.ID; id != "" {
		return id, nil
	}
	if envProject != "" {
		return envProject, nil
	}
	return "", fmt.Errorf("geminicli: onboarding did not return a Cloud project; set GOOGLE_CLOUD_PROJECT")
}

// pollOperation fetches the current state of a long-running operation by
// name.
func pollOperation(ctx context.Context, accessToken, name string) (longRunningOperation, error) {
	var op longRunningOperation
	url := fmt.Sprintf("%s/v1internal/%s", codeAssistEndpoint, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return op, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cliHeaders("") {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return op, fmt.Errorf("geminicli: poll operation: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return op, err
	}
	if resp.StatusCode != http.StatusOK {
		return op, fmt.Errorf("geminicli: poll operation failed: %s - %s", resp.Status, string(respBody))
	}
	if err := json.Unmarshal(respBody, &op); err != nil {
		return op, fmt.Errorf("geminicli: parse poll response: %w", err)
	}
	return op, nil
}

// codeAssistPost issues a JSON POST to a v1internal Code Assist method and
// returns the raw response body and status code.
func codeAssistPost(ctx context.Context, accessToken, method string, body any) ([]byte, int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	url := fmt.Sprintf("%s/v1internal:%s", codeAssistEndpoint, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cliHeaders("") {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("geminicli: %s request: %w", method, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}
