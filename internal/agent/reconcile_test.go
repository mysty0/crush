package agent

import (
	"sync"
	"sync/atomic"
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

// createInterruptedThinking writes an assistant message left with only
// reasoning content and no terminal Finish part -- a turn interrupted
// during the thinking phase, before it produced any tool call. On load
// the UI animates such a message as perpetually "thinking".
func createInterruptedThinking(t *testing.T, env fakeEnv, sessionID string) message.Message {
	t.Helper()
	msg, err := env.messages.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "let me think about this"},
		},
	})
	require.NoError(t, err)
	require.True(t, msg.IsThinking(), "fixture should look like a stuck thinking message")
	return msg
}

// TestReconcileStuckSession_FinalizesInterruptedThinking covers a turn
// cut off mid-reasoning with no tool call: it has nothing to orphan, so
// the fix count is 0, but it must still be finalized so the UI stops
// animating it as perpetually thinking.
func TestReconcileStuckSession_FinalizesInterruptedThinking(t *testing.T) {
	env := testEnv(t)
	c := newReconcileTestCoordinator(t, env, &mockSessionAgent{})

	sess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	createInterruptedThinking(t, env, sess.ID)

	fixed, err := c.ReconcileStuckSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 0, fixed, "no tool calls to reconcile")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.True(t, msgs[0].IsFinished(), "interrupted thinking must be finalized")
	require.False(t, msgs[0].IsThinking(), "must no longer render as thinking")
	require.Equal(t, message.FinishReasonCanceled, msgs[0].FinishReason())
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

// TestReconcileStuckSession_ConcurrentPassesDoNotDuplicate simulates two
// "stuck session" reconcile passes racing over the same orphaned tool
// call -- e.g. two separate crush processes sharing one database, both
// opening the same recently-used session at once. Only one of them may
// write the synthetic tool_result; a duplicate would make every
// subsequent turn fail against the provider (Anthropic rejects
// multiple tool_result blocks sharing a tool_call_id).
func TestReconcileStuckSession_ConcurrentPassesDoNotDuplicate(t *testing.T) {
	env := testEnv(t)
	c := newReconcileTestCoordinator(t, env, &mockSessionAgent{})

	sess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	createOrphanedToolCall(t, env, sess.ID, "call_1")

	const n = 8
	var wg sync.WaitGroup
	var totalFixed atomic.Int32
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fixed, err := c.ReconcileStuckSession(t.Context(), sess.ID)
			require.NoError(t, err)
			totalFixed.Add(int32(fixed))
		}(i)
	}
	wg.Wait()

	require.Equal(t, int32(1), totalFixed.Load(), "exactly one racer must fix the orphaned call")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var toolResultCount int
	for _, m := range msgs {
		toolResultCount += len(m.ToolResults())
	}
	require.Equal(t, 1, toolResultCount, "no duplicate tool_result blocks for the same tool_call_id")
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
