package agent

import (
	"slices"
	"time"
)

// TaskKind identifies which kind of background task a TaskStatus
// describes. It is the discriminator the UI switches on to open or
// cancel a task (open/cancel are inherently view-layer concerns, so
// they live at the UI edge, not on a domain object).
type TaskKind string

const (
	// TaskKindSubAgent is a sub-agent dispatched by the "agent" tool.
	TaskKindSubAgent TaskKind = "subagent"
	// TaskKindAgenticFetch is a sub-agent dispatched by "agentic_fetch".
	TaskKindAgenticFetch TaskKind = "agentic_fetch"
	// TaskKindWorkflow is a background workflow (the "Workflow" tool).
	TaskKindWorkflow TaskKind = "workflow"
	// TaskKindSchedule is a scheduled task (ScheduleCron/ScheduleWakeup).
	TaskKindSchedule TaskKind = "schedule"
	// TaskKindBash is a background bash job. Reserved for a later phase;
	// bash jobs are not yet projected into the task list.
	TaskKindBash TaskKind = "bash"
)

// TaskState is a background task's lifecycle state, normalized across
// the per-type states (SubAgentState, WorkflowRunState, ScheduleState).
type TaskState string

const (
	TaskRunning TaskState = "running"
	TaskDone    TaskState = "done"
	TaskFailed  TaskState = "failed"
	TaskStopped TaskState = "stopped"
)

// TaskRef is a stable, kind-tagged handle to a background task, usable
// across every registry.
type TaskRef struct {
	Kind TaskKind
	ID   string
}

// TaskStatus is the unified projection of a background task, merged from
// the specialized registries (sub-agent, workflow, schedule) so the UI
// can list and route every kind through one path. It carries only the
// common fields; Detail holds the original per-type status for the view
// layer to render rich, kind-specific rows and detail views.
//
// This projection is in-process only (the client/server workspace path
// stubs the task methods), so Detail's `any` never has to cross the wire.
type TaskStatus struct {
	Ref          TaskRef
	OwnerSession string // parent/origin session; "" for session-less tasks
	// ToolCallID is the chat tool call that launched the task (sub-agent,
	// agentic_fetch, workflow). Empty for schedules and bash.
	ToolCallID string
	Label      string
	State      TaskState
	StartedAt  time.Time
	FinishedAt time.Time // zero while running
	// Detail is the concrete source status (*SubAgentStatus,
	// *WorkflowStatus, *ScheduledTaskStatus) for rich UI rendering.
	Detail any
}

// asTaskStatus projects a sub-agent registry entry into the unified
// TaskStatus.
func (s SubAgentStatus) asTaskStatus() TaskStatus {
	kind := TaskKindSubAgent
	if s.ToolName == "agentic_fetch" {
		kind = TaskKindAgenticFetch
	}
	state := TaskRunning
	switch s.State {
	case SubAgentDone:
		state = TaskDone
	case SubAgentFailed:
		state = TaskFailed
	}
	return TaskStatus{
		Ref:          TaskRef{Kind: kind, ID: s.SessionID},
		OwnerSession: s.ParentSessionID,
		ToolCallID:   s.ToolCallID,
		Label:        s.Label,
		State:        state,
		StartedAt:    s.StartedAt,
		FinishedAt:   s.FinishedAt,
		Detail:       s,
	}
}

// asTaskStatus projects a workflow registry entry into the unified
// TaskStatus.
func (w WorkflowStatus) asTaskStatus() TaskStatus {
	state := TaskRunning
	switch w.State {
	case WorkflowCompleted:
		state = TaskDone
	case WorkflowFailed:
		state = TaskFailed
	case WorkflowCanceled:
		state = TaskStopped
	}
	label := w.Name
	if w.Args != "" {
		label += " · " + w.Args
	}
	return TaskStatus{
		Ref:          TaskRef{Kind: TaskKindWorkflow, ID: w.SessionID},
		OwnerSession: w.ParentSessionID,
		ToolCallID:   w.ToolCallID,
		Label:        label,
		State:        state,
		StartedAt:    w.StartedAt,
		Detail:       w,
	}
}

// asTaskStatus projects a scheduled-task registry entry into the unified
// TaskStatus.
func (s ScheduledTaskStatus) asTaskStatus() TaskStatus {
	state := TaskRunning
	if s.State == ScheduleStopped {
		state = TaskStopped
	}
	return TaskStatus{
		Ref:          TaskRef{Kind: TaskKindSchedule, ID: s.ID},
		OwnerSession: s.OriginSessionID,
		Label:        string(s.Kind),
		State:        state,
		StartedAt:    s.CreatedAt,
		Detail:       s,
	}
}

// Tasks returns the unified list of background tasks owned by
// parentSessionID -- sub-agents, workflows, and scheduled tasks it
// launched -- merged from the specialized registries, deduped by Ref,
// and sorted oldest-first. It is the single read path the UI's task
// picker consumes. (Bash jobs are added in a later phase.)
//
// Sub-agents are sourced from the registry, not from in-chat tool
// items, so a backgrounded sub-agent whose tool call already returned
// stays listed until its session actually finishes.
func (c *coordinator) Tasks(parentSessionID string) []TaskStatus {
	var out []TaskStatus
	for _, sa := range c.subAgents.list(parentSessionID) {
		// Workflow-dispatched sub-agents are represented by their
		// parent workflow row, not as top-level tasks.
		if sa.ToolName == WorkflowToolName {
			continue
		}
		out = append(out, sa.asTaskStatus())
	}
	for _, wf := range c.workflows.listByParent(parentSessionID) {
		out = append(out, wf.asTaskStatus())
	}
	for _, s := range c.schedules.list(parentSessionID) {
		out = append(out, s.asTaskStatus())
	}
	slices.SortFunc(out, func(a, b TaskStatus) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return out
}
