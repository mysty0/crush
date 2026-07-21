package agent

import (
	"context"

	"github.com/charmbracelet/crush/internal/pubsub"
)

// TaskStatusEvent signals that a background task (sub-agent, workflow,
// or scheduled task) was registered, finished, or made meaningful
// progress. It carries only the identifiers needed to route the
// change; a subscriber that needs the full detail calls
// [coordinator.Tasks] (or the owning registry's get) for the current
// snapshot, the same way [WorkflowStatusEvent] already works for
// workflows.
type TaskStatusEvent struct {
	// ParentSessionID is the session that owns the task (TaskStatus's
	// OwnerSession), so a subscriber can filter to the session it
	// cares about without resolving the ref first.
	ParentSessionID string
	// Ref identifies which task changed.
	Ref TaskRef
}

// taskStatusBroker fans out unified background-task status-change
// events to subscribers (e.g. the TUI's task picker), mirroring the
// package-level broker pattern used for retry and workflow progress.
var taskStatusBroker = pubsub.NewBroker[TaskStatusEvent]()

// SubscribeTaskStatus returns a channel that receives status-change
// events for every kind of background task (sub-agents, workflows,
// scheduled tasks) across every registry.
func SubscribeTaskStatus(ctx context.Context) <-chan pubsub.Event[TaskStatusEvent] {
	return taskStatusBroker.Subscribe(ctx)
}

// publishTaskStatus emits a status-change event for one task. Called
// by the sub-agent, workflow, and schedule registries whenever a task
// is registered, finishes, or makes meaningful progress.
func publishTaskStatus(parentSessionID string, ref TaskRef) {
	taskStatusBroker.Publish(pubsub.UpdatedEvent, TaskStatusEvent{
		ParentSessionID: parentSessionID,
		Ref:             ref,
	})
}
