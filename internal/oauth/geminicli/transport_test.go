package geminicli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractModelAndMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path       string
		wantModel  string
		wantMethod string
	}{
		{
			path:       "/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent",
			wantModel:  "gemini-3.1-pro-preview",
			wantMethod: "streamGenerateContent",
		},
		{
			path:       "/v1/models/gemini-2.5-flash:generateContent",
			wantModel:  "gemini-2.5-flash",
			wantMethod: "generateContent",
		},
		{
			path:       "/v1beta/something/else",
			wantModel:  "",
			wantMethod: "",
		},
	}
	for _, tt := range tests {
		model, method := extractModelAndMethod(tt.path)
		require.Equal(t, tt.wantModel, model, tt.path)
		require.Equal(t, tt.wantMethod, method, tt.path)
	}
}

// TestWireTransportStreaming is the core adapter test: it asserts the
// outgoing request is rewritten to the Cloud Code Assist envelope and that
// the SSE response is unwrapped for the caller.
func TestWireTransportStreaming(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// (a) URL rewritten to the v1internal streaming method with SSE.
		require.Equal(t, "/v1internal:streamGenerateContent", r.URL.Path)
		require.Equal(t, "sse", r.URL.Query().Get("alt"))

		// (c) Auth rewritten to bearer; api key removed.
		require.Equal(t, "Bearer access-123", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("x-goog-api-key"))
		require.Contains(t, r.Header.Get("User-Agent"), "GeminiCLI/")

		// (b) Body wrapped in the project/model/request envelope.
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var env struct {
			Project string          `json:"project"`
			Model   string          `json:"model"`
			Request json.RawMessage `json:"request"`
		}
		require.NoError(t, json.Unmarshal(body, &env))
		require.Equal(t, "proj-xyz", env.Project)
		require.Equal(t, "gemini-3.1-pro-preview", env.Model)
		var inner map[string]any
		require.NoError(t, json.Unmarshal(env.Request, &inner))
		require.Contains(t, inner, "contents")

		// Return an SSE stream of wrapped responses.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"index\":0}]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"index\":1}]}}\n\n")
	}))
	defer backend.Close()
	withCodeAssistEndpoint(t, backend.URL)

	rt := &WireTransport{
		Base:        backend.Client().Transport,
		AccessToken: "access-123",
		ProjectID:   "proj-xyz",
	}

	reqBody := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-3.1-pro-preview:streamGenerateContent?alt=sse",
		strings.NewReader(reqBody))
	require.NoError(t, err)
	req.Header.Set("x-goog-api-key", "SHOULD-BE-REMOVED")

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read the unwrapped SSE stream: each data: line must now be the raw
	// GenerateContentResponse, not the {response:...} envelope.
	var payloads []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		payloads = append(payloads, payload)
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &obj))
		// Must be unwrapped: no top-level "response" key remains.
		require.NotContains(t, obj, "response")
		require.Contains(t, obj, "candidates")
	}
	require.NoError(t, scanner.Err())
	require.Len(t, payloads, 2)
}

// TestWireTransportNonStreaming covers the generateContent path where the
// whole JSON body is unwrapped once.
func TestWireTransportNonStreaming(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1internal:generateContent", r.URL.Path)
		require.Empty(t, r.URL.Query().Get("alt"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"index":0}]}}`)
	}))
	defer backend.Close()
	withCodeAssistEndpoint(t, backend.URL)

	rt := &WireTransport{
		Base:        backend.Client().Transport,
		AccessToken: "tok",
		ProjectID:   "proj",
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent",
		strings.NewReader(`{"contents":[]}`))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(body, &obj))
	require.NotContains(t, obj, "response")
	require.Contains(t, obj, "candidates")
}

// TestWireTransportPassThrough ensures unrelated requests are forwarded
// untouched.
func TestWireTransportPassThrough(t *testing.T) {
	t.Parallel()

	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	rt := &WireTransport{Base: backend.Client().Transport, AccessToken: "t", ProjectID: "p"}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, backend.URL+"/v1/models", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "/v1/models", gotPath)
}

// TestSSEDefensivePassThrough asserts a data: line without a response
// envelope is passed through unchanged.
func TestSSEDefensivePassThrough(t *testing.T) {
	t.Parallel()

	in := io.NopCloser(strings.NewReader("event: ping\ndata: {\"candidates\":[]}\n\n"))
	r := newUnwrapReader(in, true)
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "event: ping\ndata: {\"candidates\":[]}\n\n", string(out))
}
