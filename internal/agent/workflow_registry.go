package agent

import (
	"context"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
)

// WorkflowRunState is the lifecycle state of a background workflow.
type WorkflowRunState string

const (
	// WorkflowRunning means the workflow is actively executing.
	WorkflowRunning WorkflowRunState = "running"
	// WorkflowCompleted means the workflow finished successfully.
	WorkflowCompleted WorkflowRunState = "completed"
	// WorkflowFailed means the workflow terminated with an error.
	WorkflowFailed WorkflowRunState = "failed"
	// WorkflowCanceled means the workflow was canceled by the user.
	WorkflowCanceled WorkflowRunState = "canceled"
)

// WorkflowPhaseStatus is the per-phase progress shown in the left pane
// of the workflow view.
type WorkflowPhaseStatus struct {
	// Name is the phase name (e.g. "Scope", "Search").
	Name string
	// Order is the sequence in which the phase was first entered,
	// used to keep the left-pane list stable.
	Order int
	// Active reports whether this is the phase currently executing.
	Active bool
	// AgentCount is the number of agent() calls dispatched in this
	// phase so far.
	AgentCount int
}

// WorkflowAgentStatus is one row in the right pane: a single
// workflow-dispatched sub-agent and its stats.
type WorkflowAgentStatus struct {
	// SessionID is the sub-agent's child session, the handle used to
	// pull live stats and (later) drill into its transcript.
	SessionID string
	// Label is the agent's descriptive label (e.g. "search:academic").
	Label string
	// Phase is the workflow phase this agent belongs to.
	Phase string
	// Provider and Model identify which model actually ran this call
	// (the workflow's default model, or a per-call override requested
	// via agent(prompt, {model=...})), so the UI can show it.
	Provider string
	Model    string
	// StartedAt marks when the agent call was dispatched.
	StartedAt time.Time
	// Done reports whether the agent call has returned.
	Done bool
}

// WorkflowStatus is a snapshot of a background workflow's progress,
// consumed by the coordinator's RunningWorkflows and the UI.
type WorkflowStatus struct {
	// SessionID is the dedicated workflow session (the cancel/view
	// handle). It parents every agent session the workflow spawns.
	SessionID string
	// ToolCallID is the Workflow tool call that launched this run, so
	// the in-place chat item can be located.
	ToolCallID string
	// ParentSessionID is the coder session that launched the workflow;
	// the completion summary is queued back into it.
	ParentSessionID string
	// Name is the workflow name (e.g. "deep-research").
	Name string
	// Args is the freeform argument the workflow was launched with.
	Args string
	// State is the current lifecycle state.
	State WorkflowRunState
	// Phases are the pipeline phases in first-seen order.
	Phases []WorkflowPhaseStatus
	// Agents are all dispatched sub-agents in dispatch order.
	Agents []WorkflowAgentStatus
	// StartedAt marks when the workflow began.
	StartedAt time.Time
	// ReportPath is the on-disk path of the final JSON report, set on
	// completion. Empty while running.
	ReportPath string
	// Summary is a short human-readable summary set on completion.
	Summary string
}

// runningWorkflow is the coordinator's internal, mutable record for one
// background workflow. It is guarded by the registry's per-entry access
// through csync.Map (whole-value get/set), so callers snapshot, mutate,
// and store back rather than mutating in place under a shared lock.
type runningWorkflow struct {
	status WorkflowStatus
	cancel context.CancelFunc
	// phaseOrder tracks the next Order value to assign to a
	// newly-seen phase.
	phaseOrder int
}

// workflowRegistry tracks all background workflows for a coordinator,
// keyed by workflow session ID. It is the single source of truth for
// RunningWorkflows, CancelWorkflow, and the two-pane view.
type workflowRegistry struct {
	entries *csync.Map[string, *runningWorkflow]
}

func newWorkflowRegistry() *workflowRegistry {
	return &workflowRegistry{
		entries: csync.NewMap[string, *runningWorkflow](),
	}
}

// register adds a new running workflow and returns nothing; the caller
// already holds the WorkflowStatus and cancel func.
func (r *workflowRegistry) register(status WorkflowStatus, cancel context.CancelFunc) {
	r.entries.Set(status.SessionID, &runningWorkflow{
		status: status,
		cancel: cancel,
	})
	publishTaskStatus(status.ParentSessionID, TaskRef{Kind: TaskKindWorkflow, ID: status.SessionID})
}

// mutate applies fn to the workflow's status under the entry, storing
// the result back. It is a no-op if the workflow is not registered.
func (r *workflowRegistry) mutate(sessionID string, fn func(w *runningWorkflow)) {
	w, ok := r.entries.Get(sessionID)
	if !ok {
		return
	}
	fn(w)
	r.entries.Set(sessionID, w)
}

// recordAgent adds (or updates) an agent row and bumps its phase's
// count. Called when a workflow agent() call is dispatched.
func (r *workflowRegistry) recordAgent(sessionID string, agent WorkflowAgentStatus) {
	r.mutate(sessionID, func(w *runningWorkflow) {
		w.ensurePhase(agent.Phase)
		w.status.Agents = append(w.status.Agents, agent)
		for i := range w.status.Phases {
			if w.status.Phases[i].Name == agent.Phase {
				w.status.Phases[i].AgentCount++
				break
			}
		}
	})
}

// markAgentDone flips the Done flag on the agent row with the given
// session ID.
func (r *workflowRegistry) markAgentDone(sessionID, agentSessionID string) {
	r.mutate(sessionID, func(w *runningWorkflow) {
		for i := range w.status.Agents {
			if w.status.Agents[i].SessionID == agentSessionID {
				w.status.Agents[i].Done = true
				break
			}
		}
	})
}

// setPhase marks phase as the active one (creating it if unseen).
func (r *workflowRegistry) setPhase(sessionID, phase string) {
	r.mutate(sessionID, func(w *runningWorkflow) {
		w.ensurePhase(phase)
		for i := range w.status.Phases {
			w.status.Phases[i].Active = w.status.Phases[i].Name == phase
		}
	})
}

// finish records the terminal state, summary, and report path, and
// clears all phases' Active flags.
func (r *workflowRegistry) finish(sessionID string, state WorkflowRunState, summary, reportPath string) {
	r.mutate(sessionID, func(w *runningWorkflow) {
		w.status.State = state
		w.status.Summary = summary
		w.status.ReportPath = reportPath
		for i := range w.status.Phases {
			w.status.Phases[i].Active = false
		}
	})
	if status, ok := r.get(sessionID); ok {
		publishTaskStatus(status.ParentSessionID, TaskRef{Kind: TaskKindWorkflow, ID: sessionID})
	}
}

// cancel invokes the workflow's cancel func. It is a no-op if the
// workflow is not registered or already finished.
func (r *workflowRegistry) cancel(sessionID string) {
	w, ok := r.entries.Get(sessionID)
	if !ok || w.cancel == nil {
		return
	}
	w.cancel()
}

// get returns a snapshot of the workflow's status.
func (r *workflowRegistry) get(sessionID string) (WorkflowStatus, bool) {
	w, ok := r.entries.Get(sessionID)
	if !ok {
		return WorkflowStatus{}, false
	}
	return w.status, true
}

// list returns snapshots of every registered workflow.
func (r *workflowRegistry) list() []WorkflowStatus {
	var out []WorkflowStatus
	for _, w := range r.entries.Seq2() {
		out = append(out, w.status)
	}
	return out
}

// listByParent returns workflows started by the given parent session,
// matching the filtering contract of subAgentRegistry.list and
// scheduleRegistry.list.
func (r *workflowRegistry) listByParent(parentSessionID string) []WorkflowStatus {
	var out []WorkflowStatus
	for _, wf := range r.list() {
		if wf.ParentSessionID == parentSessionID {
			out = append(out, wf)
		}
	}
	return out
}

// remove deletes a workflow from the registry. Called after its
// terminal state has been surfaced (e.g. once the completion summary
// has been queued), so RunningWorkflows no longer lists it.
func (r *workflowRegistry) remove(sessionID string) {
	r.entries.Del(sessionID)
}

// ensurePhase appends a phase record if the name has not been seen,
// assigning it the next Order value.
func (w *runningWorkflow) ensurePhase(phase string) {
	if phase == "" {
		return
	}
	for i := range w.status.Phases {
		if w.status.Phases[i].Name == phase {
			return
		}
	}
	w.status.Phases = append(w.status.Phases, WorkflowPhaseStatus{
		Name:  phase,
		Order: w.phaseOrder,
	})
	w.phaseOrder++
}
