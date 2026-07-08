package hooks

import (
	"context"
	"strings"
	"testing"
	"time"
)

// runLuaSource is a small helper: run a Lua hook against a payload and
// return the parsed result.
func runLuaSource(t *testing.T, source, toolInputJSON string) HookResult {
	t.Helper()
	payload := BuildPayload(EventPreToolUse, "sess-1", "/tmp/proj", "query_loki_logs", toolInputJSON)
	return runLua(context.Background(), source, payload)
}

func TestLuaHookDeny(t *testing.T) {
	src := `return { decision = "deny", reason = "no." }`
	got := runLuaSource(t, src, `{}`)
	if got.Decision != DecisionDeny {
		t.Fatalf("decision = %v, want deny", got.Decision)
	}
	if got.Reason != "no." {
		t.Fatalf("reason = %q, want %q", got.Reason, "no.")
	}
}

func TestLuaHookAllow(t *testing.T) {
	got := runLuaSource(t, `return { decision = "allow" }`, `{}`)
	if got.Decision != DecisionAllow {
		t.Fatalf("decision = %v, want allow", got.Decision)
	}
}

func TestLuaHookHalt(t *testing.T) {
	got := runLuaSource(t, `return { halt = true, reason = "stop" }`, `{}`)
	if !got.Halt {
		t.Fatal("halt = false, want true")
	}
	if got.Reason != "stop" {
		t.Fatalf("reason = %q, want %q", got.Reason, "stop")
	}
}

func TestLuaHookNoReturnIsNone(t *testing.T) {
	for _, src := range []string{
		`local x = 1`, // no return
		`return nil`,  // explicit nil
		`return {}`,   // empty table = no fields
	} {
		got := runLuaSource(t, src, `{}`)
		if got.Decision != DecisionNone {
			t.Fatalf("src %q: decision = %v, want none", src, got.Decision)
		}
	}
}

func TestLuaHookContextStringAndArray(t *testing.T) {
	got := runLuaSource(t, `return { context = "hi" }`, `{}`)
	if got.Context != "hi" {
		t.Fatalf("context = %q, want %q", got.Context, "hi")
	}
	got = runLuaSource(t, `return { context = { "a", "", "b" } }`, `{}`)
	if got.Context != "a\nb" {
		t.Fatalf("context = %q, want %q", got.Context, "a\nb")
	}
}

func TestLuaHookUpdatedInputShallowMerge(t *testing.T) {
	// Rewrite one key; the aggregator shallow-merges against tool_input.
	src := `return { updated_input = { limit = 100 } }`
	got := runLuaSource(t, src, `{"logql":"{app=\"x\"}","limit":9000}`)
	if got.UpdatedInput == "" {
		t.Fatal("updated_input empty, want a patch")
	}
	merged, err := shallowMerge(`{"logql":"{app=\"x\"}","limit":9000}`, got.UpdatedInput)
	if err != nil {
		t.Fatalf("shallowMerge: %v", err)
	}
	if !strings.Contains(merged, `"limit":100`) || !strings.Contains(merged, `"logql"`) {
		t.Fatalf("merged = %q, want limit rewritten and logql preserved", merged)
	}
}

func TestLuaHookReadsToolInput(t *testing.T) {
	// A realistic guard: deny when the logql lacks a specific label.
	src := `
		local q = hook.tool_input.logql or ""
		if not string.find(q, '=') then
			return { decision = "deny", reason = "add a label selector" }
		end
		return nil
	`
	deny := runLuaSource(t, src, `{"logql":"{}"}`)
	if deny.Decision != DecisionDeny {
		t.Fatalf("bare query: decision = %v, want deny", deny.Decision)
	}
	ok := runLuaSource(t, src, `{"logql":"{app=\"api\"}"}`)
	if ok.Decision != DecisionNone {
		t.Fatalf("labelled query: decision = %v, want none", ok.Decision)
	}
}

func TestLuaHookExposesEnvelopeFields(t *testing.T) {
	src := `
		if hook.event == "PreToolUse" and hook.tool_name == "query_loki_logs"
			and hook.session_id == "sess-1" and hook.cwd == "/tmp/proj" then
			return { decision = "allow" }
		end
		return { decision = "deny", reason = "missing fields" }
	`
	got := runLuaSource(t, src, `{}`)
	if got.Decision != DecisionAllow {
		t.Fatalf("decision = %v (%s), want allow", got.Decision, got.Reason)
	}
}

func TestLuaHookSandboxNoHostAccess(t *testing.T) {
	// os/io/require are not available; referencing them is a runtime
	// error, which is non-blocking (treated as no opinion).
	for _, src := range []string{
		`os.execute("touch /tmp/pwned")`,
		`local f = io.open("/etc/passwd")`,
		`require("os")`,
	} {
		got := runLuaSource(t, src, `{}`)
		if got.Decision != DecisionNone {
			t.Fatalf("src %q: decision = %v, want none (sandboxed error)", src, got.Decision)
		}
	}
}

func TestLuaHookParseErrorIsNonBlocking(t *testing.T) {
	got := runLuaSource(t, `return { this is not valid lua`, `{}`)
	if got.Decision != DecisionNone {
		t.Fatalf("decision = %v, want none for parse error", got.Decision)
	}
}

func TestLuaHookRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Infinite loop must be interrupted by ctx and yield no opinion.
	payload := BuildPayload(EventPreToolUse, "s", "/w", "t", `{}`)
	done := make(chan HookResult, 1)
	go func() { done <- runLua(ctx, `while true do end`, payload) }()
	select {
	case got := <-done:
		if got.Decision != DecisionNone {
			t.Fatalf("decision = %v, want none after cancel", got.Decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runLua did not return after context cancellation")
	}
}
