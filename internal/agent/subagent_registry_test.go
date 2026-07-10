package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubAgentRegistry(t *testing.T) {
	t.Parallel()

	t.Run("register then get returns the status", func(t *testing.T) {
		t.Parallel()
		r := newSubAgentRegistry()
		r.register(SubAgentStatus{
			SessionID:       "child-1",
			ParentSessionID: "parent-1",
			ToolName:        AgentToolName,
			Label:           "do the thing",
			State:           SubAgentRunning,
		})
		got, ok := r.get("child-1")
		require.True(t, ok)
		assert.Equal(t, SubAgentRunning, got.State)
		assert.Equal(t, "do the thing", got.Label)
	})

	t.Run("get on unknown session reports not found", func(t *testing.T) {
		t.Parallel()
		r := newSubAgentRegistry()
		_, ok := r.get("nope")
		require.False(t, ok)
	})

	t.Run("list only returns entries for the given parent", func(t *testing.T) {
		t.Parallel()
		r := newSubAgentRegistry()
		r.register(SubAgentStatus{SessionID: "a", ParentSessionID: "parent-1", StartedAt: time.Now()})
		r.register(SubAgentStatus{SessionID: "b", ParentSessionID: "parent-2", StartedAt: time.Now()})
		r.register(SubAgentStatus{SessionID: "c", ParentSessionID: "parent-1", StartedAt: time.Now()})

		got := r.list("parent-1")
		require.Len(t, got, 2)
		ids := []string{got[0].SessionID, got[1].SessionID}
		assert.ElementsMatch(t, []string{"a", "c"}, ids)
	})

	t.Run("list orders oldest first", func(t *testing.T) {
		t.Parallel()
		r := newSubAgentRegistry()
		now := time.Now()
		r.register(SubAgentStatus{SessionID: "later", ParentSessionID: "p", StartedAt: now.Add(time.Minute)})
		r.register(SubAgentStatus{SessionID: "earlier", ParentSessionID: "p", StartedAt: now})

		got := r.list("p")
		require.Len(t, got, 2)
		assert.Equal(t, "earlier", got[0].SessionID)
		assert.Equal(t, "later", got[1].SessionID)
	})

	t.Run("finish sets terminal state and finished time", func(t *testing.T) {
		t.Parallel()
		r := newSubAgentRegistry()
		r.register(SubAgentStatus{SessionID: "a", ParentSessionID: "p", State: SubAgentRunning})

		r.finish("a", SubAgentFailed, "boom")

		got, ok := r.get("a")
		require.True(t, ok)
		assert.Equal(t, SubAgentFailed, got.State)
		assert.Equal(t, "boom", got.Error)
		assert.False(t, got.FinishedAt.IsZero())
	})

	t.Run("finish on unknown session is a no-op", func(t *testing.T) {
		t.Parallel()
		r := newSubAgentRegistry()
		r.finish("nope", SubAgentDone, "")
		_, ok := r.get("nope")
		require.False(t, ok)
	})

	t.Run("remove deletes the entry", func(t *testing.T) {
		t.Parallel()
		r := newSubAgentRegistry()
		r.register(SubAgentStatus{SessionID: "a", ParentSessionID: "p"})
		r.remove("a")
		_, ok := r.get("a")
		require.False(t, ok)
	})
}

// TestRunSubAgent_RegistersInSubAgentRegistry verifies that runSubAgent
// registers the sub-agent as running before dispatch and marks it
// done/failed synchronously once it returns -- i.e. before the linger
// goroutine that eventually removes the entry runs (subAgentLingerAfterFinish).
func TestRunSubAgent_RegistersInSubAgentRegistry(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("success marks the entry done", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var sawRunning bool
		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			status, ok := coord.subAgents.get(call.SessionID)
			sawRunning = ok && status.State == SubAgentRunning
			return agentResultWithText("done"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "do something",
			SessionTitle:   "Test Session",
			ToolName:       AgentToolName,
			Label:          "do something",
		})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		assert.True(t, sawRunning, "sub-agent must be registered as running before its turn dispatches")

		list := coord.subAgents.list(parentSession.ID)
		require.Len(t, list, 1)
		assert.Equal(t, SubAgentDone, list[0].State)
		assert.Equal(t, AgentToolName, list[0].ToolName)
		assert.Equal(t, "do something", list[0].Label)
		assert.Equal(t, providerID, list[0].Provider)
	})

	t.Run("failure marks the entry failed with the error", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, assert.AnError
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "do something",
			SessionTitle:   "Test Session",
			ToolName:       AgentToolName,
		})
		require.NoError(t, err) // runSubAgent reports failures via ToolResponse, not error.

		list := coord.subAgents.list(parentSession.ID)
		require.Len(t, list, 1)
		assert.Equal(t, SubAgentFailed, list[0].State)
		assert.NotEmpty(t, list[0].Error)
	})
}

func TestAgentListTool(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})

	t.Run("no sub-agents", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "empty-parent")
		resp, err := coord.agentListTool().Run(ctx, fantasy.ToolCall{ID: "call-1", Input: "{}"})
		require.NoError(t, err)
		assert.Contains(t, resp.Content, "No sub-agents")
	})

	t.Run("lists a registered sub-agent", func(t *testing.T) {
		t.Parallel()
		coord.subAgents.register(SubAgentStatus{
			SessionID:       "child-1",
			ParentSessionID: "parent-1",
			ToolName:        AgentToolName,
			Label:           "investigate the bug",
			State:           SubAgentRunning,
			StartedAt:       time.Now(),
		})

		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "parent-1")
		resp, err := coord.agentListTool().Run(ctx, fantasy.ToolCall{ID: "call-1", Input: "{}"})
		require.NoError(t, err)
		assert.Contains(t, resp.Content, "child-1")
		assert.Contains(t, resp.Content, "investigate the bug")
		assert.Contains(t, resp.Content, "running")
	})

	t.Run("missing session in context is an error", func(t *testing.T) {
		t.Parallel()
		_, err := coord.agentListTool().Run(t.Context(), fantasy.ToolCall{ID: "call-1", Input: "{}"})
		require.Error(t, err)
	})
}

func TestAgentProgressTool(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})

	t.Run("unknown session returns an error response", func(t *testing.T) {
		t.Parallel()
		input, err := json.Marshal(AgentProgressParams{SessionID: "nope"})
		require.NoError(t, err)
		resp, err := coord.agentProgressTool().Run(t.Context(), fantasy.ToolCall{ID: "call-1", Input: string(input)})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
	})

	t.Run("missing session_id is an error response", func(t *testing.T) {
		t.Parallel()
		resp, err := coord.agentProgressTool().Run(t.Context(), fantasy.ToolCall{ID: "call-1", Input: "{}"})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
	})

	t.Run("reports registered status and message stats", func(t *testing.T) {
		t.Parallel()
		childSession, err := env.sessions.Create(t.Context(), "Child")
		require.NoError(t, err)

		coord.subAgents.register(SubAgentStatus{
			SessionID:       childSession.ID,
			ParentSessionID: "parent-1",
			ToolName:        AgentToolName,
			Label:           "investigate the bug",
			Provider:        "test-provider",
			Model:           "test-model",
			State:           SubAgentRunning,
			StartedAt:       time.Now(),
		})

		input, err := json.Marshal(AgentProgressParams{SessionID: childSession.ID})
		require.NoError(t, err)
		resp, err := coord.agentProgressTool().Run(t.Context(), fantasy.ToolCall{ID: "call-1", Input: string(input)})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		assert.Contains(t, resp.Content, "investigate the bug")
		assert.Contains(t, resp.Content, "test-provider/test-model")
		assert.Contains(t, resp.Content, "running")
	})
}
