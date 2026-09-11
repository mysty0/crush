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

	"github.com/google/uuid"
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

// requireUUID asserts s looks like a canonical RFC 4122 UUID string, the
// form uuid.NewString produces.
func requireUUID(t *testing.T, s string) {
	t.Helper()
	require.NotEmpty(t, s)
	_, err := uuid.Parse(s)
	require.NoError(t, err, "expected a UUID, got %q", s)
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

		// (d) Client-Metadata is never sent: the real client's binary
		// contains no such header name.
		require.Empty(t, r.Header.Get("Client-Metadata"))

		// (b) Body wrapped in the Cloud Code Assist envelope.
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var env struct {
			Project      string          `json:"project"`
			Model        string          `json:"model"`
			Request      json.RawMessage `json:"request"`
			RequestID    string          `json:"requestId"`
			UserAgent    string          `json:"userAgent"`
			UserPromptID string          `json:"userPromptId"`
		}
		require.NoError(t, json.Unmarshal(body, &env))
		require.Equal(t, "proj-xyz", env.Project)
		require.Equal(t, "gemini-3.1-pro-preview", env.Model)
		requireUUID(t, env.RequestID)
		require.Equal(t, r.Header.Get("User-Agent"), env.UserAgent)
		require.Equal(t, "prompt-abc", env.UserPromptID)
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
	ctx := WithUserPromptID(context.Background(), "prompt-abc")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
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

// TestWireTransportEnvelopeIdentityFields covers the envelope fields that
// identify the caller and the logical prompt: requestId must be a fresh
// UUID per HTTP call, userAgent must mirror the header, and userPromptId
// must be absent entirely (not blank) when no turn id is on the context.
func TestWireTransportEnvelopeIdentityFields(t *testing.T) {
	var envelopes []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var env map[string]any
		require.NoError(t, json.Unmarshal(body, &env))
		envelopes = append(envelopes, env)

		// Regression guards on the request headers.
		require.Contains(t, r.Header.Get("User-Agent"), "GeminiCLI/")
		require.Equal(t, env["userAgent"], r.Header.Get("User-Agent"))
		require.Empty(t, r.Header.Get("Client-Metadata"))
		_, hasClientMetadata := r.Header["Client-Metadata"]
		require.False(t, hasClientMetadata, "Client-Metadata must not be sent")

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[]}}`)
	}))
	defer backend.Close()
	withCodeAssistEndpoint(t, backend.URL)

	rt := &WireTransport{
		Base:        backend.Client().Transport,
		AccessToken: "tok",
		ProjectID:   "proj",
	}

	roundTrip := func(ctx context.Context) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent",
			strings.NewReader(`{"contents":[]}`))
		require.NoError(t, err)
		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	// No prompt id on the context: the key must be omitted, not blank.
	roundTrip(context.Background())
	require.NotContains(t, envelopes[0], "userPromptId")
	requireUUID(t, envelopes[0]["requestId"].(string))
	require.NotEmpty(t, envelopes[0]["userAgent"])

	// A prompt id on the context is reported verbatim, and stays stable
	// across the several requests one turn makes...
	ctx := WithUserPromptID(context.Background(), "turn-42")
	roundTrip(ctx)
	roundTrip(ctx)
	require.Equal(t, "turn-42", envelopes[1]["userPromptId"])
	require.Equal(t, "turn-42", envelopes[2]["userPromptId"])

	// ...while requestId is fresh for every HTTP call.
	requireUUID(t, envelopes[1]["requestId"].(string))
	requireUUID(t, envelopes[2]["requestId"].(string))
	require.NotEqual(t, envelopes[1]["requestId"], envelopes[2]["requestId"])
	require.NotEqual(t, envelopes[0]["requestId"], envelopes[1]["requestId"])
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

// TestWireTransportUsesIdentityEndpoint proves inference traffic honors
// Identity.Endpoint, not just the discovery/onboarding calls.
//
// This is the regression that caused Antigravity to be rate limited
// immediately on every request: its inference calls were being sent to
// Gemini CLI's host, which resolves the same token to a different
// backend project with no usable quota. Discovery honoring the override
// while the transport ignored it would reintroduce exactly that split.
func TestWireTransportUsesIdentityEndpoint(t *testing.T) {
	wrong := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("inference went to the package-default endpoint, not the identity's Endpoint")
	}))
	defer wrong.Close()
	withCodeAssistEndpoint(t, wrong.URL)

	right := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1internal:generateContent", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"index":0}]}}`)
	}))
	defer right.Close()

	rt := &WireTransport{
		Base:        right.Client().Transport,
		AccessToken: "access-123",
		ProjectID:   "proj-xyz",
		Identity: Identity{
			Product:  "Antigravity",
			Version:  "1.1.1",
			Endpoint: right.URL,
		},
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-3.1-pro-preview:generateContent",
		strings.NewReader(`{"contents":[]}`))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestWireTransportUnaliasesModelID proves the alias never escapes to
// the backend.
//
// Claude models are renamed on the way into fantasy so its google
// provider does not divert them (see WrapCodeAssistWireFormat), which
// means genai builds the request URL from the alias. The envelope the
// backend actually reads must carry the real model id -- an alias here
// would be rejected as an unknown model, trading a 404 for a 400.
func TestWireTransportUnaliasesModelID(t *testing.T) {
	var gotModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var env struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &env))
		gotModel = env.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"index":0}]}}`)
	}))
	defer backend.Close()
	withCodeAssistEndpoint(t, backend.URL)

	rt := &WireTransport{
		Base:        backend.Client().Transport,
		AccessToken: "access-123",
		ProjectID:   "proj-xyz",
	}

	alias := aliasModelID("claude-sonnet-4-6")
	require.NotEqual(t, "claude-sonnet-4-6", alias, "precondition: the id must actually be aliased")

	req, err := http.NewRequest(http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/"+alias+":generateContent",
		strings.NewReader(`{"contents":[]}`))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, "claude-sonnet-4-6", gotModel,
		"the envelope must carry the real model id, not the alias")
}
