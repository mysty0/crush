package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/permission"
)

// planModeBlockMessage is returned to the model when it tries to use a
// mutating tool while plan mode is active. It explains what to do instead so
// the model stops and proposes a plan rather than retrying blindly.
const planModeBlockMessage = "Plan mode is active. You must not modify anything yet — no file edits, writes, downloads, or shell commands. Investigate using read-only tools, then present a concise, numbered plan and wait for the user to review it and exit plan mode before making any changes."

// planModeTool wraps a fantasy.AgentTool and blocks it while plan mode is
// active. Plan mode is read at Run time so toggling it mid-session takes
// effect immediately without rebuilding the agent. Read-only tools pass
// through untouched.
type planModeTool struct {
	inner       fantasy.AgentTool
	permissions permission.Service
}

// wrapToolsWithPlanMode wraps every blocking-eligible tool so it is gated on
// plan mode. Tools that can never mutate are returned unwrapped. When perms
// is nil the slice is returned unchanged.
func wrapToolsWithPlanMode(ts []fantasy.AgentTool, perms permission.Service) []fantasy.AgentTool {
	if perms == nil {
		return ts
	}
	out := make([]fantasy.AgentTool, len(ts))
	for i, tool := range ts {
		if permission.PlanModeBlocksTool(tool.Info().Name) {
			out[i] = &planModeTool{inner: tool, permissions: perms}
			continue
		}
		out[i] = tool
	}
	return out
}

func (p *planModeTool) Info() fantasy.ToolInfo {
	return p.inner.Info()
}

func (p *planModeTool) ProviderOptions() fantasy.ProviderOptions {
	return p.inner.ProviderOptions()
}

func (p *planModeTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	p.inner.SetProviderOptions(opts)
}

func (p *planModeTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if p.permissions.PlanMode() {
		resp := fantasy.NewTextErrorResponse(planModeBlockMessage)
		// Only block this call; let the model see the message and adjust.
		// Do not end the turn so it can keep researching or present a plan.
		return resp, nil
	}
	return p.inner.Run(ctx, call)
}
