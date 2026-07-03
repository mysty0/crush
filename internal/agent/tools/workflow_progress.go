package tools

import (
	"context"

	"github.com/charmbracelet/crush/internal/pubsub"
)

// WorkflowProgressEvent carries the accumulated progress transcript of
// a running Workflow tool call, so the UI can stream it live before the
// tool returns. It mirrors BashProgressEvent: Output is the full
// transcript so far (not just the latest line), so a subscriber that
// missed earlier lossy updates still renders the correct current state.
type WorkflowProgressEvent struct {
	SessionID  string
	ToolCallID string
	Output     string
}

// workflowProgressBroker fans out workflow progress events to
// subscribers (e.g. the TUI), mirroring the pattern used for bash
// progress.
var workflowProgressBroker = pubsub.NewBroker[WorkflowProgressEvent]()

// SubscribeWorkflowProgress returns a channel that receives progress
// events from running Workflow tool calls.
func SubscribeWorkflowProgress(ctx context.Context) <-chan pubsub.Event[WorkflowProgressEvent] {
	return workflowProgressBroker.Subscribe(ctx)
}

// PublishWorkflowProgress emits the accumulated progress transcript for
// a running workflow. Delivery is lossy by design: dropped intermediate
// updates are harmless because each event carries the full transcript.
func PublishWorkflowProgress(sessionID, toolCallID, output string) {
	workflowProgressBroker.Publish(pubsub.UpdatedEvent, WorkflowProgressEvent{
		SessionID:  sessionID,
		ToolCallID: toolCallID,
		Output:     output,
	})
}

// WorkflowStatusEvent signals that a background workflow's status
// (phases, agents, or lifecycle state) changed, prompting the UI to
// refresh the two-pane view and the picker list. It carries only
// identifiers; the UI queries the coordinator for the current
// snapshot.
type WorkflowStatusEvent struct {
	// WorkflowSessionID is the workflow's dedicated session ID.
	WorkflowSessionID string
	// ToolCallID is the Workflow tool call that launched the run.
	ToolCallID string
}

// workflowStatusBroker fans out workflow status-change events.
var workflowStatusBroker = pubsub.NewBroker[WorkflowStatusEvent]()

// SubscribeWorkflowStatus returns a channel that receives workflow
// status-change events.
func SubscribeWorkflowStatus(ctx context.Context) <-chan pubsub.Event[WorkflowStatusEvent] {
	return workflowStatusBroker.Subscribe(ctx)
}

// PublishWorkflowStatus emits a status-change event for a workflow.
func PublishWorkflowStatus(workflowSessionID, toolCallID string) {
	workflowStatusBroker.Publish(pubsub.UpdatedEvent, WorkflowStatusEvent{
		WorkflowSessionID: workflowSessionID,
		ToolCallID:        toolCallID,
	})
}
