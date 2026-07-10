package agent

import (
	"slices"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
)

// SubAgentState is a sub-agent invocation's lifecycle state.
type SubAgentState string

const (
	// SubAgentRunning means the sub-agent's turn is still in flight.
	SubAgentRunning SubAgentState = "running"
	// SubAgentDone means the sub-agent finished successfully.
	SubAgentDone SubAgentState = "done"
	// SubAgentFailed means the sub-agent's turn returned an error.
	SubAgentFailed SubAgentState = "failed"
)

// SubAgentStatus is a snapshot of one sub-agent invocation dispatched
// via the agent, agentic_fetch, or Workflow tools, returned by the
// AgentList and AgentProgress tools and consumable by the UI the same
// way WorkflowStatus is.
type SubAgentStatus struct {
	// SessionID is the sub-agent's own session -- the handle used to
	// pull live progress (message/tool-call count) and to resume it
	// via the agent tool's resume_session_id.
	SessionID string
	// ParentSessionID is the session that dispatched this sub-agent.
	// AgentList filters by this so a caller only sees sub-agents it
	// (not a sibling session) launched.
	ParentSessionID string
	// ToolCallID is the tool call that launched this sub-agent.
	ToolCallID string
	// ToolName is the dispatching tool: "agent", "agentic_fetch", or
	// "Workflow".
	ToolName string
	// Label is a short human-readable description of the task: the
	// task prompt for the agent tool, the URL for agentic_fetch, the
	// workflow-assigned label for a Workflow-dispatched agent.
	Label string
	// Provider and Model identify which model is actually running
	// this sub-agent.
	Provider string
	Model    string
	// StartedAt marks when the sub-agent's turn was dispatched.
	StartedAt time.Time
	// FinishedAt marks when it completed. Zero while running.
	FinishedAt time.Time
	State      SubAgentState
	// Error is a short, best-effort note about why the sub-agent
	// failed. Empty unless State is SubAgentFailed.
	Error string
}

// runningSubAgent is the registry's mutable, internal record for one
// sub-agent invocation.
type runningSubAgent struct {
	status SubAgentStatus
}

// subAgentRegistry tracks every sub-agent invocation dispatched via
// runSubAgent (the agent, agentic_fetch, and Workflow tools' common
// choke point), keyed by the sub-agent's own session ID. It is the
// source of truth for the AgentList and AgentProgress tools.
type subAgentRegistry struct {
	entries *csync.Map[string, *runningSubAgent]
}

func newSubAgentRegistry() *subAgentRegistry {
	return &subAgentRegistry{entries: csync.NewMap[string, *runningSubAgent]()}
}

// register adds a newly-dispatched sub-agent.
func (r *subAgentRegistry) register(status SubAgentStatus) {
	r.entries.Set(status.SessionID, &runningSubAgent{status: status})
}

// finish marks a sub-agent's terminal state. No-op if the sub-agent is
// not registered.
func (r *subAgentRegistry) finish(sessionID string, state SubAgentState, errMsg string) {
	e, ok := r.entries.Get(sessionID)
	if !ok {
		return
	}
	e.status.State = state
	e.status.Error = errMsg
	e.status.FinishedAt = time.Now()
	r.entries.Set(sessionID, e)
}

// get returns a snapshot of one sub-agent's status.
func (r *subAgentRegistry) get(sessionID string) (SubAgentStatus, bool) {
	e, ok := r.entries.Get(sessionID)
	if !ok {
		return SubAgentStatus{}, false
	}
	return e.status, true
}

// list returns every sub-agent dispatched directly from
// parentSessionID, oldest first.
func (r *subAgentRegistry) list(parentSessionID string) []SubAgentStatus {
	var out []SubAgentStatus
	for e := range r.entries.Seq() {
		if e.status.ParentSessionID == parentSessionID {
			out = append(out, e.status)
		}
	}
	slices.SortFunc(out, func(a, b SubAgentStatus) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return out
}

// remove deletes a finished sub-agent from the registry so it no
// longer appears in AgentList. Called after its terminal state has
// lingered briefly (see subAgentLingerAfterFinish).
func (r *subAgentRegistry) remove(sessionID string) {
	r.entries.Del(sessionID)
}
