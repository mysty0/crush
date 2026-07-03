package chat

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Workflow Tool
// -----------------------------------------------------------------------------

// WorkflowToolMessageItem is a message item that represents a Workflow
// tool call. While the workflow runs it streams a live progress
// transcript (Scope -> Search -> Fetch -> Verify -> Synthesize) via the
// PartialOutput mechanism, then shows the final report.
type WorkflowToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*WorkflowToolMessageItem)(nil)

// NewWorkflowToolMessageItem creates a new [WorkflowToolMessageItem].
func NewWorkflowToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	t := &WorkflowToolMessageItem{}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &WorkflowToolRenderContext{}, canceled)
	// A workflow tool call is marked Finished as soon as its input is
	// parsed, long before the multi-phase run completes. Keep spinning
	// until a result arrives (or the turn is canceled) so the animation
	// reflects that the workflow is still running.
	t.spinningFunc = func(state SpinningState) bool {
		return !state.HasResult() && !state.IsCanceled()
	}
	return t
}

// WorkflowToolRenderContext renders Workflow tool messages.
type WorkflowToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (w *WorkflowToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)

	var params agent.WorkflowParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	name := strings.TrimSpace(params.Name)
	if name == "" {
		name = "workflow"
	}

	// While the workflow is still running (no result yet), show a
	// header plus the streamed progress transcript so far.
	if !opts.HasResult() && !opts.IsCanceled() {
		if opts.Compact {
			return pendingTool(sty, "Workflow", opts.Anim, opts.Compact)
		}
		header := toolHeader(sty, opts.Status, "Workflow", cappedWidth, opts, name)
		if opts.Anim != nil {
			if animView := opts.Anim.Render(); animView != "" {
				header += " " + animView
			}
		}
		if opts.PartialOutput == "" {
			return header
		}
		bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
		body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.PartialOutput, bodyWidth, opts.ExpandedContent))
		return joinToolParts(header, body)
	}

	header := toolHeader(sty, opts.Status, "Workflow", cappedWidth, opts, name)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() || opts.Result.Content == "" {
		return header
	}

	// The final result is a JSON report; render it as-is in a plain
	// output block (it is structured data, not markdown).
	bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
	body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.Content, bodyWidth, opts.ExpandedContent))
	return joinToolParts(header, body)
}
