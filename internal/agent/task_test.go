package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestTasks_IncludesBackgroundedSubAgent is the regression guard for the
// bug this unification fixed: a sub-agent whose tool call already
// returned (it was backgrounded) is still running in the registry, and
// must appear in the unified task list so the UI can keep it selectable.
func TestTasks_IncludesBackgroundedSubAgent(t *testing.T) {
	t.Parallel()
	c := &coordinator{
		subAgents: newSubAgentRegistry(),
		workflows: newWorkflowRegistry(),
		schedules: newScheduleRegistry(),
	}
	c.subAgents.register(SubAgentStatus{
		SessionID:       "sub-1",
		ParentSessionID: "parent",
		ToolCallID:      "call-1",
		ToolName:        "agent",
		Label:           "do the thing",
		State:           SubAgentRunning,
		StartedAt:       time.Now(),
	})

	tasks := c.Tasks("parent")
	require.Len(t, tasks, 1)
	got := tasks[0]
	require.Equal(t, TaskRef{Kind: TaskKindSubAgent, ID: "sub-1"}, got.Ref)
	require.Equal(t, "parent", got.OwnerSession)
	require.Equal(t, "call-1", got.ToolCallID)
	require.Equal(t, "do the thing", got.Label)
	require.Equal(t, TaskRunning, got.State)
}

// TestTasks_FiltersByOwningSession verifies the unified list only
// returns tasks launched by the given session, and drops workflow-
// dispatched sub-agents (represented by their workflow row instead).
func TestTasks_FiltersByOwningSession(t *testing.T) {
	t.Parallel()
	c := &coordinator{
		subAgents: newSubAgentRegistry(),
		workflows: newWorkflowRegistry(),
		schedules: newScheduleRegistry(),
	}
	c.subAgents.register(SubAgentStatus{SessionID: "a", ParentSessionID: "mine", ToolName: "agent", State: SubAgentRunning})
	c.subAgents.register(SubAgentStatus{SessionID: "b", ParentSessionID: "other", ToolName: "agent", State: SubAgentRunning})
	c.subAgents.register(SubAgentStatus{SessionID: "c", ParentSessionID: "mine", ToolName: "Workflow", State: SubAgentRunning})

	tasks := c.Tasks("mine")
	require.Len(t, tasks, 1)
	require.Equal(t, "a", tasks[0].Ref.ID)
}

// TestSubAgentStatus_asTaskStatus checks the kind mapping for the two
// agent-tool dispatch types and the state normalization.
func TestSubAgentStatus_asTaskStatus(t *testing.T) {
	t.Parallel()
	fetch := SubAgentStatus{SessionID: "x", ToolName: "agentic_fetch", State: SubAgentFailed}.asTaskStatus()
	require.Equal(t, TaskKindAgenticFetch, fetch.Ref.Kind)
	require.Equal(t, TaskFailed, fetch.State)

	sub := SubAgentStatus{SessionID: "y", ToolName: "agent", State: SubAgentDone}.asTaskStatus()
	require.Equal(t, TaskKindSubAgent, sub.Ref.Kind)
	require.Equal(t, TaskDone, sub.State)
}
