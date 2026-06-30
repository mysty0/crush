package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// ccClaudeCodeIdentity is the exact system-prompt identity line that
// Anthropic's subscription-OAuth (Claude Code) endpoint requires as a
// DISCRETE first system block. When a request authenticates with a
// Claude Pro/Max OAuth token, the API returns a terse
// 429 rate_limit_error ("Error") unless `system` is an array whose first
// block is exactly this string. The official Claude Code client sends
// `system: [{identity}, {rest}]`; fantasy emits the whole prompt as a
// single block, so we split it here at the HTTP layer.
const ccClaudeCodeIdentity = "You are Claude Code, Anthropic's official CLI for Claude."

// ccSystemSplitTransport rewrites outgoing /v1/messages request bodies so
// the Claude Code identity line is the discrete first system block, which
// the subscription-OAuth endpoint requires. It handles three cases:
//
//   - The system prompt is a single block (or string) that begins with the
//     identity: it is split into [{identity}, {rest}].
//   - injectIdentity is set and the identity is absent entirely (e.g. the
//     title, summary, and sub-agent prompts, which don't carry it): the
//     identity is prepended as a new first block.
//   - The identity is already the discrete first block: left unchanged.
//
// It is a no-op for any request whose system prompt does not need rewriting
// (e.g. normal Anthropic API-key usage when injectIdentity is false), and is
// idempotent: an already-correct body passes through unchanged, so SDK
// retries are safe.
type ccSystemSplitTransport struct {
	base http.RoundTripper

	// injectIdentity prepends the identity when it is missing. It must be
	// true only for the subscription-OAuth provider; injecting it onto plain
	// Anthropic API-key traffic would misrepresent those requests.
	injectIdentity bool
}

func (t *ccSystemSplitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Body == nil || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		return base.RoundTrip(req)
	}

	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return base.RoundTrip(req)
	}

	if rewritten, ok := ccEnsureIdentityFirst(body, t.injectIdentity); ok {
		body = rewritten
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	// Ensure retries regenerate the (already-rewritten) body.
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return base.RoundTrip(req)
}

// ccEnsureIdentityFirst parses an Anthropic /v1/messages JSON body and makes
// the Claude Code identity line the discrete first system block. It returns
// the rewritten body and true when it changed the body, or (nil, false) when
// no change is needed or possible. All non-system fields are preserved
// verbatim.
//
// When inject is false it only splits an existing sole block that begins with
// the identity (the original behavior). When inject is true it additionally
// prepends the identity as a new first block if the identity is not already
// present at the front.
func ccEnsureIdentityFirst(body []byte, inject bool) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	raw, ok := payload["system"]

	// No system field at all. With injection on, add one carrying just the
	// identity so the OAuth endpoint is satisfied.
	if !ok {
		if !inject {
			return nil, false
		}
		return ccWriteSystem(payload, []map[string]json.RawMessage{ccIdentityBlock()})
	}

	// Already correct: the first block is exactly the identity. Leave as-is
	// so the rewrite is idempotent across SDK retries.
	if ccFirstBlockIsIdentity(raw) {
		return nil, false
	}

	text, cacheControl, ok := ccExtractSoleSystemText(raw)
	if ok && strings.HasPrefix(text, ccClaudeCodeIdentity) {
		// Single block that starts with the identity: split it into
		// [{identity}, {rest}], keeping the cache breakpoint on the bulk.
		rest := strings.TrimLeft(text[len(ccClaudeCodeIdentity):], "\n")
		if rest == "" {
			return nil, false
		}
		restBlock := map[string]json.RawMessage{
			"type": json.RawMessage(`"text"`),
			"text": ccMustMarshal(rest),
		}
		if cacheControl != nil {
			restBlock["cache_control"] = cacheControl
		}
		return ccWriteSystem(payload, []map[string]json.RawMessage{ccIdentityBlock(), restBlock})
	}

	// The identity is absent. With injection on, prepend it as a new first
	// block, preserving every existing block after it.
	if !inject {
		return nil, false
	}
	existing, ok := ccSystemBlocks(raw)
	if !ok {
		return nil, false
	}
	blocks := append([]map[string]json.RawMessage{ccIdentityBlock()}, existing...)
	return ccWriteSystem(payload, blocks)
}

// ccWriteSystem marshals blocks into payload["system"] and returns the
// re-encoded body.
func ccWriteSystem(payload map[string]json.RawMessage, blocks []map[string]json.RawMessage) ([]byte, bool) {
	newSystem, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}
	payload["system"] = newSystem
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ccIdentityBlock returns a fresh {type:"text", text:identity} block.
func ccIdentityBlock() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"type": json.RawMessage(`"text"`),
		"text": ccMustMarshal(ccClaudeCodeIdentity),
	}
}

// ccFirstBlockIsIdentity reports whether the system field is an array whose
// first text block equals the identity exactly.
func ccFirstBlockIsIdentity(raw json.RawMessage) bool {
	blocks, ok := ccSystemBlocks(raw)
	if !ok || len(blocks) == 0 {
		return false
	}
	var text string
	if err := json.Unmarshal(blocks[0]["text"], &text); err != nil {
		return false
	}
	return text == ccClaudeCodeIdentity
}

// ccSystemBlocks decodes the system field as an array of blocks. A plain
// string form is wrapped into a single text block so callers can treat both
// shapes uniformly.
func ccSystemBlocks(raw json.RawMessage) ([]map[string]json.RawMessage, bool) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return []map[string]json.RawMessage{{
			"type": json.RawMessage(`"text"`),
			"text": ccMustMarshal(asString),
		}}, true
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// ccExtractSoleSystemText returns the text of a `system` field when it is
// either a plain string or an array containing exactly one text block. The
// second return is the block's cache_control (nil for the string form). The
// "exactly one block" guard makes the rewrite idempotent: an already-split
// [{identity},{rest}] body has two blocks and is left untouched.
func ccExtractSoleSystemText(raw json.RawMessage) (string, json.RawMessage, bool) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil, true
	}

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) != 1 {
		return "", nil, false
	}
	var text string
	if err := json.Unmarshal(blocks[0]["text"], &text); err != nil {
		return "", nil, false
	}
	return text, blocks[0]["cache_control"], true
}

func ccMustMarshal(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
