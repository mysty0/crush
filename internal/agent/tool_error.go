package agent

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

// ToolExecutionError tags an error returned from a tool's Run so the run
// handler can distinguish a tool-originated halt from a provider or model
// error. Fantasy treats any error returned by a tool as critical and aborts
// the whole run; without this tag the abort would be reported to the user as
// a generic "Provider Error" even though the failure came from a tool.
type ToolExecutionError struct {
	ToolName string
	Err      error
}

func (e *ToolExecutionError) Error() string {
	return fmt.Sprintf("tool %q failed: %v", e.ToolName, e.Err)
}

func (e *ToolExecutionError) Unwrap() error {
	return e.Err
}

// errorTaggingTool wraps a fantasy.AgentTool so that any non-nil error
// returned from Run is tagged with the tool's name via ToolExecutionError.
// Soft failures returned as error tool results (fantasy.NewTextErrorResponse)
// are left untouched — they are fed back to the model and never reach here.
type errorTaggingTool struct {
	inner fantasy.AgentTool
}

func (t *errorTaggingTool) Info() fantasy.ToolInfo {
	return t.inner.Info()
}

func (t *errorTaggingTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *errorTaggingTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *errorTaggingTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := t.inner.Run(ctx, call)
	if err != nil {
		// Preserve cancellation semantics: a canceled run is handled
		// separately by the caller via errors.Is, which unwraps through
		// ToolExecutionError, so tagging here is safe.
		return resp, &ToolExecutionError{ToolName: t.inner.Info().Name, Err: err}
	}
	return resp, nil
}

// wrapToolsWithErrorTagging tags each tool so that errors it returns are
// attributed to it. This must be the outermost wrapper so it also captures
// errors surfaced by the hook and plan-mode decorators.
func wrapToolsWithErrorTagging(ts []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(ts))
	for i, tool := range ts {
		out[i] = &errorTaggingTool{inner: tool}
	}
	return out
}
