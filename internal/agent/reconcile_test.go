package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// newReconcileTestCoordinator builds a coordinator with every registry
// ReconcileStuckSession and CancelAll touch initialized, backed by a
// real sqlite session/message service pair.
func newReconcileTestCoordinator(t *testing.T, env fakeEnv, current SessionAgent) *coordinator {
	t.Helper()
	return &coordinator{
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: current,
		taskAgents:   csync.NewMap[string, SessionAgent](),
		workflows:    newWorkflowRegistry(),
		schedules:    newScheduleRegistry(),
		subAgents:    newSubAgentRegistry(),
	}
}

// createOrphanedToolCall writes an assistant message with a single
// unfinished-run tool call (no matching tool result) into sessionID,
// simulating a turn interrupted mid-flight.
func createOrphanedToolCall(t *testing.T, env fakeEnv, sessionID, toolCallID string) message.Message {
	t.Helper()
	msg, err := env.messages.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "let me check"},
			message.ToolCall{
				ID:       toolCallID,
				Name:     AgentToolName,
				Input:    `{"prompt":"do something"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)
	return msg
}

func TestReconcileStuckSession_FixesOrphanedToolCall(t *testing.T) {
	env := testEnv(t)
	c := newReconcileTestCoordinator(t, env, &mockSessionAgent{})

	sess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	createOrphanedToolCall(t, env, sess.ID, "call_1")

	fixed, err := c.ReconcileStuckSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, fixed)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	assistant := msgs[0]
	require.Equal(t, message.FinishReasonCanceled, assistant.FinishReason())

	toolMsg := msgs[1]
	require.Equal(t, message.Tool, toolMsg.Role)
	results := toolMsg.ToolResults()
	require.Len(t, results, 1)
	require.Equal(t, "call_1", results[0].ToolCallID)
	require.True(t, results[0].IsError)
}

func TestReconcileStuckSession_SkipsBusySession(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	createOrphanedToolCall(t, env, sess.ID, "call_1")

	c := newReconcileTestCoordinator(t, env, &mockSessionAgent{busySessionID: sess.ID})

	fixed, err := c.ReconcileStuckSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 0, fixed)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1, "a genuinely busy session must be left untouched")
	require.False(t, msgs[0].IsFinished())
}

func TestReconcileStuckSession_RecursesIntoChildSessions(t *testing.T) {
	env := testEnv(t)
	c := newReconcileTestCoordinator(t, env, &mockSessionAgent{})

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	assistant := createOrphanedToolCall(t, env, parent.ID, "call_1")

	childID := env.sessions.CreateAgentToolSessionID(assistant.ID, "call_1")
	_, err = env.sessions.CreateTaskSession(t.Context(), childID, parent.ID, "sub-agent")
	require.NoError(t, err)
	createOrphanedToolCall(t, env, childID, "call_2")

	fixed, err := c.ReconcileStuckSession(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Equal(t, 2, fixed)

	childMsgs, err := env.messages.List(t.Context(), childID)
	require.NoError(t, err)
	require.Len(t, childMsgs, 2)
	require.Equal(t, message.FinishReasonCanceled, childMsgs[0].FinishReason())
}

func TestReconcileStuckSession_SkipsLiveWorkflowSubtree(t *testing.T) {
	env := testEnv(t)
	c := newReconcileTestCoordinator(t, env, &mockSessionAgent{})

	workflowSess, err := env.sessions.Create(t.Context(), "workflow")
	require.NoError(t, err)
	c.workflows.register(WorkflowStatus{
		SessionID: workflowSess.ID,
		State:     WorkflowRunning,
	}, func() {})

	childID := env.sessions.CreateAgentToolSessionID(workflowSess.ID, "agent-1")
	_, err = env.sessions.CreateTaskSession(t.Context(), childID, workflowSess.ID, "workflow agent")
	require.NoError(t, err)
	createOrphanedToolCall(t, env, childID, "call_1")

	fixed, err := c.ReconcileStuckSession(t.Context(), workflowSess.ID)
	require.NoError(t, err)
	require.Equal(t, 0, fixed, "a live workflow's subtree must be left untouched")
}

func TestCoordinatorCancelAll(t *testing.T) {
	env := testEnv(t)
	current := &mockSessionAgent{}
	taskAgent := &mockSessionAgent{}
	c := newReconcileTestCoordinator(t, env, current)
	c.taskAgents.Set("read/x/y", taskAgent)

	var workflowCanceled bool
	c.workflows.register(WorkflowStatus{SessionID: "wf-1", State: WorkflowRunning}, func() {
		workflowCanceled = true
	})
	c.schedules.register(ScheduledTaskStatus{ID: "sched-1", State: ScheduleActive}, func() {})

	c.CancelAll()

	require.True(t, current.cancelAllCalled, "CancelAll must cancel the main coder agent")
	require.True(t, taskAgent.cancelAllCalled, "CancelAll must cancel every task/sub-agent instance")
	require.True(t, workflowCanceled, "CancelAll must cancel running workflows")
	sched, ok := c.schedules.get("sched-1")
	require.True(t, ok)
	require.Equal(t, ScheduleStopped, sched.State, "CancelAll must stop active scheduled tasks")
}
