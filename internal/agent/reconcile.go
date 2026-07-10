package agent

import (
	"context"

	"github.com/charmbracelet/crush/internal/message"
)

// reconcileInterruptedContent is the tool result content written for a
// tool call left unfinished by an interrupted run.
const reconcileInterruptedContent = "Interrupted: Crush was closed or restarted while this was running."

// ReconcileStuckSession implements Coordinator.
func (c *coordinator) ReconcileStuckSession(ctx context.Context, sessionID string) (int, error) {
	return c.reconcileStuckSession(ctx, sessionID, make(map[string]bool), false)
}

// reconcileStuckSession is the recursive worker behind
// ReconcileStuckSession. ancestorLive is true when a session further
// up the tree was found to have a live run, in which case its entire
// subtree is left untouched: a workflow's nested sub-agent sessions
// are not independently tracked in the coordinator's busy-state
// registries, so the only safe signal that they might still be
// running is that their live-tracked ancestor is.
func (c *coordinator) reconcileStuckSession(ctx context.Context, sessionID string, visited map[string]bool, ancestorLive bool) (int, error) {
	if visited[sessionID] {
		return 0, nil
	}
	visited[sessionID] = true

	live := ancestorLive || c.sessionHasLiveRun(sessionID)

	var fixed int
	children, err := c.sessions.ListChildren(ctx, sessionID)
	if err != nil {
		return fixed, err
	}
	for _, child := range children {
		n, err := c.reconcileStuckSession(ctx, child.ID, visited, live)
		fixed += n
		if err != nil {
			return fixed, err
		}
	}

	if live {
		return fixed, nil
	}

	n, err := c.reconcileSessionMessages(ctx, sessionID)
	return fixed + n, err
}

// sessionHasLiveRun reports whether sessionID has a run actively
// tracked by any of the coordinator's busy-state registries: the main
// coder agent, any task/sub-agent instance dispatched via the "agent"
// tool, or a background workflow.
func (c *coordinator) sessionHasLiveRun(sessionID string) bool {
	if c.IsSessionBusy(sessionID) {
		return true
	}
	for taskAgent := range c.taskAgents.Seq() {
		if taskAgent != nil && taskAgent.IsSessionBusy(sessionID) {
			return true
		}
	}
	if wf, ok := c.workflows.get(sessionID); ok && wf.State == WorkflowRunning {
		return true
	}
	return false
}

// reconcileSessionMessages scans one session's own message history
// (not its descendants) for tool calls with no matching tool result
// and writes a synthetic, error-flagged result for each, mirroring
// the cleanup a live run's own error path performs (see
// sessionAgent.Run's post-stream error handling). Any assistant
// message left without a terminal Finish part is marked
// FinishReasonCanceled. Returns the number of tool calls reconciled.
func (c *coordinator) reconcileSessionMessages(ctx context.Context, sessionID string) (int, error) {
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return 0, err
	}

	knownToolResultIDs := make(map[string]struct{})
	for _, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		for _, tr := range m.ToolResults() {
			knownToolResultIDs[tr.ToolCallID] = struct{}{}
		}
	}

	var fixed int
	for _, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		toolCalls := m.ToolCalls()
		if len(toolCalls) == 0 {
			continue
		}

		var orphaned bool
		for _, tc := range toolCalls {
			if _, ok := knownToolResultIDs[tc.ID]; ok {
				continue
			}
			orphaned = true
			_, err := c.messages.Create(ctx, sessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{message.ToolResult{
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    reconcileInterruptedContent,
					IsError:    true,
				}},
			})
			if err != nil {
				return fixed, err
			}
			fixed++
		}

		if orphaned && !m.IsFinished() {
			m.AddFinish(message.FinishReasonCanceled, "Interrupted", "Crush was closed or restarted while this turn was in progress.")
			if err := c.messages.Update(ctx, m); err != nil {
				return fixed, err
			}
		}
	}
	return fixed, nil
}
