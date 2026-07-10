package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/tools"
)

const (
	// AgentListToolName lists sub-agents dispatched from the current
	// session.
	AgentListToolName = "AgentList"
	// AgentProgressToolName reports detailed live progress for one
	// sub-agent.
	AgentProgressToolName = "AgentProgress"
)

//go:embed templates/agent_list.md
var agentListDescription string

//go:embed templates/agent_progress.md
var agentProgressDescription string

// AgentListParams are the parameters for the AgentList tool (none).
type AgentListParams struct{}

// AgentProgressParams are the parameters for the AgentProgress tool.
type AgentProgressParams struct {
	SessionID string `json:"session_id" description:"The sub-agent's session ID, from AgentList or a prior agent tool call's response."`
}

// agentListTool implements the AgentList tool: lists every sub-agent
// dispatched from the current session via the agent, agentic_fetch, or
// Workflow tools, running or recently finished.
func (c *coordinator) agentListTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		AgentListToolName,
		agentListDescription,
		func(ctx context.Context, _ AgentListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			list := c.subAgents.list(sessionID)
			if len(list) == 0 {
				return fantasy.NewTextResponse("No sub-agents dispatched from this session."), nil
			}
			var b strings.Builder
			for _, s := range list {
				fmt.Fprintf(&b, "- %s [%s/%s] %q", s.SessionID, s.ToolName, s.State, truncateForList(s.Label, 60))
				switch s.State {
				case SubAgentRunning:
					fmt.Fprintf(&b, " -- running for %s", time.Since(s.StartedAt).Round(time.Second))
				case SubAgentFailed:
					fmt.Fprintf(&b, " -- failed after %s: %s", s.FinishedAt.Sub(s.StartedAt).Round(time.Second), truncateForList(s.Error, 80))
				default:
					fmt.Fprintf(&b, " -- finished in %s", s.FinishedAt.Sub(s.StartedAt).Round(time.Second))
				}
				b.WriteString("\n")
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)
}

// agentProgressTool implements the AgentProgress tool: detailed live
// progress for one sub-agent, computed from its persisted session and
// message history the same way the workflow view's per-agent stats are.
func (c *coordinator) agentProgressTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		AgentProgressToolName,
		agentProgressDescription,
		func(ctx context.Context, params AgentProgressParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.SessionID == "" {
				return fantasy.NewTextErrorResponse("session_id is required"), nil
			}
			status, ok := c.subAgents.get(params.SessionID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown sub-agent session %q", params.SessionID)), nil
			}

			var b strings.Builder
			fmt.Fprintf(&b, "Session: %s\nDispatched by: %s\nTask: %s\nModel: %s/%s\nState: %s\n",
				status.SessionID, status.ToolName, status.Label, status.Provider, status.Model, status.State)
			switch status.State {
			case SubAgentRunning:
				fmt.Fprintf(&b, "Running for: %s\n", time.Since(status.StartedAt).Round(time.Second))
			default:
				fmt.Fprintf(&b, "Duration: %s\n", status.FinishedAt.Sub(status.StartedAt).Round(time.Second))
				if status.Error != "" {
					fmt.Fprintf(&b, "Error: %s\n", status.Error)
				}
			}

			msgs, err := c.messages.List(ctx, params.SessionID)
			if err != nil {
				return fantasy.NewTextResponse(b.String() + "\n(failed to load message history: " + err.Error() + ")"), nil
			}

			var (
				toolCalls    int
				lastToolCall string
				lastText     string
			)
			for i := range msgs {
				msg := &msgs[i]
				for _, tc := range msg.ToolCalls() {
					toolCalls++
					if !tc.Finished {
						lastToolCall = tc.Name + " (in progress)"
					} else {
						lastToolCall = tc.Name
					}
				}
				if text := strings.TrimSpace(msg.Content().Text); text != "" {
					lastText = text
				}
			}
			fmt.Fprintf(&b, "Tool calls so far: %d\n", toolCalls)
			if lastToolCall != "" {
				fmt.Fprintf(&b, "Most recent tool call: %s\n", lastToolCall)
			}
			if lastText != "" {
				fmt.Fprintf(&b, "Latest output:\n%s\n", truncateForList(lastText, 500))
			}

			return fantasy.NewTextResponse(b.String()), nil
		},
	)
}
