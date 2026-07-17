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
// (not its descendants) and heals whatever an interrupted run left
// behind. It writes a synthetic, error-flagged result for every tool
// call with no matching result (mirroring the cleanup a live run's own
// error path performs, see sessionAgent.Run's post-stream error
// handling), and marks every assistant message with no terminal Finish
// part as FinishReasonCanceled. The latter also covers a turn
// interrupted during the reasoning phase, before any tool call, which
// the UI would otherwise animate as a perpetual "thinking" indicator on
// the next load. Returns the number of orphaned tool calls reconciled.
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

		// Write a synthetic result for any tool call this turn left
		// unanswered (interrupted after the model asked for a tool but
		// before the result was recorded).
		for _, tc := range m.ToolCalls() {
			if _, ok := knownToolResultIDs[tc.ID]; ok {
				continue
			}
			_, created, err := c.messages.CreateToolResultIfAbsent(ctx, sessionID, message.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    reconcileInterruptedContent,
				IsError:    true,
			})
			if err != nil {
				return fixed, err
			}
			// A concurrent reconcile pass (this process or another
			// sharing the same database) may have already written a
			// result between our read of msgs above and now; only count
			// results we actually created.
			if created {
				fixed++
			}
		}

		// Finalize any assistant message the interrupted run left
		// without a terminal Finish part. This covers both an
		// interrupted tool call (handled above) and a turn cut off
		// mid-reasoning with no tool call at all -- the latter would
		// otherwise render as an eternally animating "thinking"
		// indicator on the next load. Safe because the caller only
		// reaches here for a session with no live run.
		if !m.IsFinished() {
			m.AddFinish(message.FinishReasonCanceled, "Interrupted", "Crush was closed or restarted while this turn was in progress.")
			if err := c.messages.Update(ctx, m); err != nil {
				return fixed, err
			}
		}
	}
	return fixed, nil
}
