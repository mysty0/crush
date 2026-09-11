package chat

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

// -----------------------------------------------------------------------------
// Bash Tool
// -----------------------------------------------------------------------------

// BashToolMessageItem is a message item that represents a bash tool call.
type BashToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*BashToolMessageItem)(nil)

// NewBashToolMessageItem creates a new [BashToolMessageItem].
func NewBashToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
	workingDir string,
) ToolMessageItem {
	t := &BashToolMessageItem{}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &BashToolRenderContext{workingDir: workingDir}, canceled)
	return t
}

// BashToolRenderContext renders bash tool messages.
type BashToolRenderContext struct {
	workingDir string
}

// RenderTool implements the [ToolRenderer] interface.
func (b *BashToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)

	var params tools.BashParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		params.Command = "failed to parse command"
	}

	// While the command is still running (no result yet), show the
	// command header plus any output streamed so far so the user sees
	// progress in real time. The tool call is marked Finished as soon as
	// its input is parsed — well before execution completes — so we key
	// this on the absence of a result rather than IsPending().
	if !opts.HasResult() && !opts.IsCanceled() {
		if opts.Compact || params.Command == "" {
			return pendingTool(sty, "Bash", opts.Anim, opts.Compact)
		}
		cmd := strings.ReplaceAll(params.Command, "\n", " ")
		cmd = strings.ReplaceAll(cmd, "\t", "    ")
		header := toolHeader(sty, opts.Status, "Bash", cappedWidth, opts, cmd)
		// Keep the running animation next to the command header so the
		// command line stays visible while it runs. We render just the
		// bare animation (not a full "Bash" pending line) to avoid a
		// second, duplicate Bash header.
		if opts.Anim != nil {
			if animView := opts.Anim.Render(); animView != "" {
				header += " " + animView
			}
		}
		if opts.PartialOutput == "" {
			return header
		}
		// Output is streaming in: show the header + output.
		bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
		body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.PartialOutput, bodyWidth, opts.ExpandedContent))
		return joinToolParts(header, body)
	}

	// Check if this is a background job.
	var meta tools.BashResponseMetadata
	if opts.HasResult() {
		_ = json.Unmarshal([]byte(opts.Result.Metadata), &meta)
	}

	if meta.Background {
		description := cmp.Or(meta.Description, params.Command)
		content := "Command: " + params.Command + "\n" + opts.Result.Content
		return renderJobTool(sty, opts, cappedWidth, "Start", meta.ShellID, description, content)
	}

	// Regular bash command.
	cmd := params.Command
	if !opts.ExpandedContent {
		cmd = strings.ReplaceAll(cmd, "\n", " ")
	}
	cmd = strings.ReplaceAll(cmd, "\t", "    ")
	cmd = common.StripBashDisplayPrefix(cmd, b.workingDir)
	if highlighted, err := common.SyntaxHighlightLexerName(sty, cmd, "bash", nil); err == nil {
		cmd = highlighted
	}
	toolParams := []string{cmd}
	if params.RunInBackground {
		toolParams = append(toolParams, "background", "true")
	}

	header := toolHeader(sty, opts.Status, "Bash", cappedWidth, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() {
		return header
	}

	output := meta.Output
	if output == "" && opts.Result.Content != tools.BashNoOutput {
		output = opts.Result.Content
	}
	if output == "" {
		return header
	}

	bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
	body := sty.Tool.Body.Render(toolOutputPlainContent(sty, output, bodyWidth, opts.ExpandedContent))
	return joinToolParts(header, body)
}

// -----------------------------------------------------------------------------
// Job Output Tool
// -----------------------------------------------------------------------------

// JobOutputToolMessageItem is a message item for job_output tool calls.
type JobOutputToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*JobOutputToolMessageItem)(nil)

// NewJobOutputToolMessageItem creates a new [JobOutputToolMessageItem].
func NewJobOutputToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &JobOutputToolRenderContext{}, canceled)
}

// JobOutputToolRenderContext renders job_output tool messages.
type JobOutputToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (j *JobOutputToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)

	var params tools.JobOutputParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		if !opts.HasResult() && !opts.IsCanceled() {
			return pendingTool(sty, "Job", opts.Anim, opts.Compact)
		}
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}

	// The tool call is marked Finished as soon as its arguments are
	// parsed, well before job_output actually returns -- especially
	// with wait=true, which can block for up to MaxJobOutputWaitSeconds
	// -- so IsPending() cannot be trusted to mean "done." Key off the
	// absence of a result instead, same fix Bash already has, and show
	// a live spinner + elapsed timer next to the header so a long wait
	// doesn't look frozen.
	if !opts.HasResult() && !opts.IsCanceled() {
		if opts.Compact || params.ShellID == "" {
			return pendingTool(sty, "Job", opts.Anim, opts.Compact)
		}
		return runningJobHeader(sty, opts, cappedWidth, "Output", params.ShellID)
	}

	var description string
	if opts.Result.Metadata != "" {
		var meta tools.JobOutputResponseMetadata
		if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
			description = cmp.Or(meta.Description, meta.Command)
		}
	}

	return renderJobTool(sty, opts, cappedWidth, "Output", params.ShellID, description, opts.Result.Content)
}

// -----------------------------------------------------------------------------
// Job Kill Tool
// -----------------------------------------------------------------------------

// JobKillToolMessageItem is a message item for job_kill tool calls.
type JobKillToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*JobKillToolMessageItem)(nil)

// NewJobKillToolMessageItem creates a new [JobKillToolMessageItem].
func NewJobKillToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &JobKillToolRenderContext{}, canceled)
}

// JobKillToolRenderContext renders job_kill tool messages.
type JobKillToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (j *JobKillToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)

	var params tools.JobKillParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		if !opts.HasResult() && !opts.IsCanceled() {
			return pendingTool(sty, "Job", opts.Anim, opts.Compact)
		}
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}

	// Same "Finished means arguments parsed, not done running" trap as
	// job_output -- see JobOutputToolRenderContext.RenderTool.
	if !opts.HasResult() && !opts.IsCanceled() {
		if opts.Compact || params.ShellID == "" {
			return pendingTool(sty, "Job", opts.Anim, opts.Compact)
		}
		return runningJobHeader(sty, opts, cappedWidth, "Kill", params.ShellID)
	}

	var description string
	if opts.Result.Metadata != "" {
		var meta tools.JobKillResponseMetadata
		if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
			description = cmp.Or(meta.Description, meta.Command)
		}
	}

	return renderJobTool(sty, opts, cappedWidth, "Kill", params.ShellID, description, opts.Result.Content)
}

// runningJobHeader renders the header for a job tool (job_output,
// job_kill) that is genuinely still executing -- no result yet -- with a
// live spinner appended next to it. Mirrors Bash's treatment of the same
// "Finished means arguments parsed, not done running" trap: without this,
// a long wait=true job_output call falls through to the generic
// "Waiting for tool response..." fallback and looks frozen instead of
// showing progress or an elapsed timer.
func runningJobHeader(sty *styles.Styles, opts *ToolRenderOpts, width int, action, shellID string) string {
	header := jobHeader(sty, opts.Status, action, shellID, "", width)
	if opts.Anim != nil {
		if animView := opts.Anim.Render(); animView != "" {
			header += " " + animView
		}
	}
	return header
}

// renderJobTool renders a job-related tool with the common pattern:
// header → nested check → early state → body.
func renderJobTool(sty *styles.Styles, opts *ToolRenderOpts, width int, action, shellID, description, content string) string {
	header := jobHeader(sty, opts.Status, action, shellID, description, width)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if content == "" {
		return header
	}

	bodyWidth := width - toolBodyLeftPaddingTotal
	body := sty.Tool.Body.Render(toolOutputPlainContent(sty, content, bodyWidth, opts.ExpandedContent))
	return joinToolParts(header, body)
}

// jobHeader builds a header for job-related tools.
// Format: "● Job (Action) PID shellID description..."
func jobHeader(sty *styles.Styles, status ToolStatus, action, shellID, description string, width int) string {
	icon := toolIcon(sty, status)
	jobPart := sty.Tool.JobToolName.Render("Job")
	actionPart := sty.Tool.JobAction.Render("(" + action + ")")
	pidPart := sty.Tool.JobPID.Render("PID " + shellID)

	prefix := fmt.Sprintf("%s %s %s %s", icon, jobPart, actionPart, pidPart)

	if description == "" {
		return prefix
	}

	prefixWidth := lipgloss.Width(prefix)
	availableWidth := width - prefixWidth - 1
	if availableWidth < 10 {
		return prefix
	}

	truncatedDesc := ansi.Truncate(description, availableWidth, "…")
	return prefix + " " + sty.Tool.JobDescription.Render(truncatedDesc)
}

// joinToolParts joins header and body with a blank line separator.
func joinToolParts(header, body string) string {
	return strings.Join([]string{header, "", body}, "\n")
}
