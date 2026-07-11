package geminicli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// WireTransport adapts Crush's standard Gemini requests (as emitted by the
// google.golang.org/genai SDK) to the Cloud Code Assist wire format used by
// cloudcode-pa.googleapis.com.
//
// The genai SDK targets {baseURL}/{apiVersion}/models/{model}:{method} with
// a plain GenerateContentRequest body and x-goog-api-key auth, and expects
// raw GenerateContentResponse objects back. The Cloud Code Assist backend
// instead exposes {endpoint}/v1internal:{method}, wraps the request body in
// a {project, model, request} envelope, wraps each response in a {response}
// envelope, and authenticates with a bearer token. WireTransport performs
// that rewrite on the way out and unwraps the response on the way back.
type WireTransport struct {
	// Base is the underlying RoundTripper. When nil,
	// http.DefaultTransport is used.
	Base http.RoundTripper
	// AccessToken is the Cloud Code Assist bearer token.
	AccessToken string
	// ProjectID is the discovered Cloud project id attached to every
	// request envelope.
	ProjectID string
	// Identity identifies the calling client product (see Identity).
	// Zero value falls back to GeminiCLIIdentity.
	Identity Identity
}

// RoundTrip rewrites an outgoing genai request into the Cloud Code Assist
// envelope, forwards it, and unwraps the response body.
func (t *WireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	model, method := extractModelAndMethod(req.URL.Path)
	if method == "" {
		// Not a generate call we recognize; forward unchanged so we do
		// not break unrelated requests.
		return base.RoundTrip(req)
	}
	stream := method == "streamGenerateContent"

	// Read and parse the original Gemini request body.
	var original json.RawMessage
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("geminicli: read request body: %w", err)
		}
		if len(raw) > 0 {
			original = json.RawMessage(raw)
		}
	}

	// Build the Cloud Code Assist request envelope.
	envelope := map[string]any{
		"project": t.ProjectID,
		"model":   model,
		"request": original,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("geminicli: marshal request envelope: %w", err)
	}

	// Clone the request so the caller's copy is untouched, then rewrite
	// the URL, headers, and body.
	outReq := req.Clone(reqContext(req))
	newURL := fmt.Sprintf("%s/v1internal:%s", codeAssistEndpoint, method)
	if stream {
		newURL += "?alt=sse"
	}
	u, err := url.Parse(newURL)
	if err != nil {
		return nil, fmt.Errorf("geminicli: parse rewritten URL: %w", err)
	}
	outReq.URL = u
	outReq.Host = u.Host

	outReq.Body = io.NopCloser(bytes.NewReader(body))
	outReq.ContentLength = int64(len(body))
	outReq.Header.Set("Content-Length", strconv.Itoa(len(body)))

	outReq.Header.Del("x-goog-api-key")
	outReq.Header.Del("X-Goog-Api-Key")
	outReq.Header.Set("Authorization", "Bearer "+t.AccessToken)
	outReq.Header.Set("Content-Type", "application/json")
	id := t.Identity
	if id == (Identity{}) {
		id = GeminiCLIIdentity
	}
	for k, v := range cliHeaders(model, id) {
		outReq.Header.Set(k, v)
	}

	resp, err := base.RoundTrip(outReq)
	if err != nil {
		return nil, err
	}

	// Unwrap the response envelope. The transformed length is unknown, so
	// drop the length hints.
	resp.Body = newUnwrapReader(resp.Body, stream)
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

// reqContext returns the request's context so the cloned request keeps
// cancellation semantics.
func reqContext(req *http.Request) context.Context {
	if ctx := req.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// extractModelAndMethod parses a genai request path of the form
// /{apiVersion}/models/{model}:{method} and returns the model id and the
// method (e.g. "streamGenerateContent"). It returns empty strings when the
// path does not match.
func extractModelAndMethod(path string) (model, method string) {
	idx := strings.Index(path, "/models/")
	if idx < 0 {
		return "", ""
	}
	rest := path[idx+len("/models/"):]
	// rest is "{model}:{method}" possibly followed by more path segments.
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	colon := strings.LastIndexByte(rest, ':')
	if colon < 0 {
		return "", ""
	}
	return rest[:colon], rest[colon+1:]
}

// newUnwrapReader wraps a Cloud Code Assist response body so the caller
// reads plain GenerateContentResponse objects. When stream is true the body
// is an SSE stream whose data: lines are unwrapped individually; otherwise
// the whole JSON body is unwrapped once.
func newUnwrapReader(rc io.ReadCloser, stream bool) io.ReadCloser {
	if stream {
		return &sseUnwrapReader{
			src:    bufio.NewReader(rc),
			closer: rc,
		}
	}
	return &jsonUnwrapReader{src: rc}
}

// unwrapResponse extracts the ".response" sub-object from a Cloud Code
// Assist envelope. When the payload has no "response" field it is returned
// unchanged (defensive against unexpected shapes).
func unwrapResponse(payload []byte) []byte {
	var env struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return payload
	}
	if len(env.Response) == 0 {
		return payload
	}
	return env.Response
}

// sseUnwrapReader transforms an SSE stream, unwrapping the ".response"
// object from every data: line while passing all other lines through
// unchanged. It reads whole lines so it is robust to chunk boundaries.
type sseUnwrapReader struct {
	src    *bufio.Reader
	closer io.Closer
	buf    bytes.Buffer
	done   bool
}

// Read implements io.Reader, refilling from the source one line at a time.
func (r *sseUnwrapReader) Read(p []byte) (int, error) {
	for r.buf.Len() == 0 && !r.done {
		line, err := r.src.ReadBytes('\n')
		if len(line) > 0 {
			r.buf.Write(transformSSELine(line))
		}
		if err != nil {
			r.done = true
			if err != io.EOF {
				// Surface non-EOF errors after draining buffered bytes.
				if r.buf.Len() == 0 {
					return 0, err
				}
			}
			break
		}
	}
	if r.buf.Len() == 0 {
		return 0, io.EOF
	}
	return r.buf.Read(p)
}

// Close closes the underlying body.
func (r *sseUnwrapReader) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

// transformSSELine unwraps a single SSE line. Only "data:" lines carrying a
// JSON envelope are rewritten; blank lines, comments, and event/id lines are
// passed through verbatim so the framing is preserved.
func transformSSELine(line []byte) []byte {
	// Split trailing newline(s) so we can restore them exactly.
	content := line
	var suffix []byte
	if i := bytes.IndexByte(content, '\n'); i >= 0 {
		suffix = content[i:]
		content = content[:i]
	}
	trimmed := bytes.TrimRight(content, "\r")
	carriage := content[len(trimmed):]

	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 {
		return line
	}
	unwrapped := unwrapResponse(payload)
	if bytes.Equal(unwrapped, payload) {
		return line
	}

	var out bytes.Buffer
	out.WriteString("data: ")
	out.Write(unwrapped)
	out.Write(carriage)
	out.Write(suffix)
	return out.Bytes()
}

// jsonUnwrapReader lazily reads the whole (non-streaming) response body and
// unwraps the ".response" object on first Read.
type jsonUnwrapReader struct {
	src    io.ReadCloser
	buf    bytes.Buffer
	loaded bool
	err    error
}

// Read implements io.Reader, loading and unwrapping the body on demand.
func (r *jsonUnwrapReader) Read(p []byte) (int, error) {
	if !r.loaded {
		r.loaded = true
		raw, err := io.ReadAll(r.src)
		if err != nil {
			r.err = err
		} else {
			r.buf.Write(unwrapResponse(raw))
		}
	}
	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

// Close closes the underlying body.
func (r *jsonUnwrapReader) Close() error {
	return r.src.Close()
}
