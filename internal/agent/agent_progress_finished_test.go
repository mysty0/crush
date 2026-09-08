package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentProgressAnswersForReapedSubAgent reproduces an observed
// failure: an agent polling on a multi-minute timer asked about a
// sub-agent that had finished ~4.5 minutes earlier, delivered its
// report, and been reaped from the registry (which retains a finished
// entry for only subAgentLingerAfterFinish). AgentProgress answered
// "unknown background task", the agent read that as "it never ran", and
// re-dispatched a duplicate of work it was already holding the result
// of.
//
// A finished task's session outlives its registry entry, so the tool
// must answer from the session and say plainly that the task completed.
func TestAgentProgressAnswersForReapedSubAgent(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})

	parent, err := env.sessions.Create(t.Context(), "Parent conversation")
	require.NoError(t, err)

	// A real sub-agent session ID is "<messageID>$$<toolCallID>".
	subID := env.sessions.CreateAgentToolSessionID("msg-abc", "toolu_reaped")
	_, err = env.sessions.CreateTaskSession(t.Context(), subID, parent.ID, "Reverse-engineer the movement state")
	require.NoError(t, err)

	finalReport := "# Report\n\nThe decoded state machine is as follows."
	assistant := message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: finalReport}},
	}
	created, err := env.messages.Create(t.Context(), subID, assistant)
	require.NoError(t, err)
	created.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, env.messages.Update(t.Context(), created))

	// The sub-agent ran and was reaped: nothing is in the registry.
	_, stillTracked := coord.subAgents.get(subID)
	require.False(t, stillTracked, "precondition: the finished entry has been reaped")

	input, err := json.Marshal(AgentProgressParams{SessionID: subID})
	require.NoError(t, err)
	resp, err := coord.agentProgressTool().Run(t.Context(), fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)

	require.False(t, resp.IsError,
		"a finished task must not be reported as an error; that reads as 'never existed'")
	assert.NotContains(t, resp.Content, "unknown background task",
		"the reaped task must not be denied")
	assert.Contains(t, resp.Content, "finished",
		"the response must state the task completed")
	assert.Contains(t, resp.Content, "completed normally",
		"the stored finish reason must be surfaced")
	assert.Contains(t, resp.Content, "decoded state machine",
		"the final output must be recoverable from the transcript")
	assert.Contains(t, resp.Content, "follow-up",
		"the response must point at where the result was delivered")
}

// TestAgentProgressUnknownIDExplainsTheFormat covers the genuinely
// not-found case. The response must not read as proof that a task died:
// trimming the "$$<toolCallID>" suffix from a sub-agent session ID
// leaves a bare message ID that matches no session, which is exactly
// the mistake that made a live agent conclude its sub-agent had
// vanished.
func TestAgentProgressUnknownIDExplainsTheFormat(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})

	input, err := json.Marshal(AgentProgressParams{SessionID: "msg-abc"})
	require.NoError(t, err)
	resp, err := coord.agentProgressTool().Run(t.Context(), fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)

	assert.Contains(t, resp.Content, "$$",
		"an unrecognized ID must explain the full session ID format")
	assert.Contains(t, resp.Content, "not that a task failed",
		"an unrecognized ID must not read as a failed or vanished task")
}

// TestAgentProgressStillReportsRunningTasks guards that the fallback
// did not displace the live path: a running sub-agent must still be
// reported from the registry, with its live elapsed time.
func TestAgentProgressStillReportsRunningTasks(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})

	child, err := env.sessions.Create(t.Context(), "Child")
	require.NoError(t, err)
	coord.subAgents.register(SubAgentStatus{
		SessionID:       child.ID,
		ParentSessionID: "parent-1",
		ToolName:        AgentToolName,
		Label:           "investigate the bug",
		Provider:        "test-provider",
		Model:           "test-model",
		State:           SubAgentRunning,
		StartedAt:       time.Now(),
	})

	input, err := json.Marshal(AgentProgressParams{SessionID: child.ID})
	require.NoError(t, err)
	resp, err := coord.agentProgressTool().Run(context.Background(), fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)

	require.False(t, resp.IsError)
	assert.Contains(t, resp.Content, "investigate the bug")
	assert.Contains(t, resp.Content, "running")
}
