package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/charmbracelet/crush/internal/oauth/geminicli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSessionAgent is a minimal mock for the SessionAgent interface.
type mockSessionAgent struct {
	model     Model
	runFunc   func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	cancelled []string
	// busySessionID, when non-empty, makes IsSessionBusy report true for
	// that one session ID (used to test resume-while-busy rejection).
	busySessionID string
	// cancelAllCalled records whether CancelAll was invoked.
	cancelAllCalled bool
}

func (m *mockSessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	return m.runFunc(ctx, call)
}

func (m *mockSessionAgent) BeginAccepted(sessionID string) *AcceptedRun {
	return &AcceptedRun{sessionID: sessionID}
}

func (m *mockSessionAgent) Model() Model                        { return m.model }
func (m *mockSessionAgent) SetModels(large, small Model)        {}
func (m *mockSessionAgent) SetTools(tools []fantasy.AgentTool)  {}
func (m *mockSessionAgent) SetSystemPrompt(systemPrompt string) {}
func (m *mockSessionAgent) ArmReady(n int) func()               { return func() {} }
func (m *mockSessionAgent) Cancel(sessionID string) {
	m.cancelled = append(m.cancelled, sessionID)
}

func (m *mockSessionAgent) CancelKeepQueue(sessionID string) {
	m.cancelled = append(m.cancelled, sessionID)
}
func (m *mockSessionAgent) CancelAll() { m.cancelAllCalled = true }
func (m *mockSessionAgent) IsSessionBusy(sessionID string) bool {
	return m.busySessionID != "" && m.busySessionID == sessionID
}
func (m *mockSessionAgent) IsBusy() bool                                { return false }
func (m *mockSessionAgent) QueuedPrompts(sessionID string) int          { return 0 }
func (m *mockSessionAgent) QueuedPromptsList(sessionID string) []string { return nil }
func (m *mockSessionAgent) ClearQueue(sessionID string)                 {}
func (m *mockSessionAgent) Summarize(context.Context, string, fantasy.ProviderOptions) error {
	return nil
}
func (m *mockSessionAgent) GenerateTitle(context.Context, string, string) {}

// newTestCoordinator creates a minimal coordinator for unit testing runSubAgent.
func newTestCoordinator(t *testing.T, env fakeEnv, providerID string, providerCfg config.ProviderConfig) *coordinator {
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, providerCfg)
	return &coordinator{
		cfg:       cfg,
		sessions:  env.sessions,
		messages:  env.messages,
		subAgents: newSubAgentRegistry(),
		// currentAgent is a minimal non-nil stand-in so a backgrounded
		// sub-agent's completion (which queues a follow-up via
		// coordinator.Run -> UpdateModels -> currentAgent.SetModels)
		// doesn't panic on a nil interface; UpdateModels still fails
		// gracefully right after with errCoderAgentNotConfigured since
		// no agent config is set up here.
		currentAgent: &mockSessionAgent{},
	}
}

// newMockAgent creates a mockSessionAgent with the given provider and run function.
func newMockAgent(providerID string, maxTokens int64, runFunc func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)) *mockSessionAgent {
	return &mockSessionAgent{
		model: Model{
			CatwalkCfg: catwalk.Model{
				DefaultMaxTokens: maxTokens,
			},
			ModelCfg: config.SelectedModel{
				Provider: providerID,
			},
		},
		runFunc: runFunc,
	}
}

// agentResultWithText creates a minimal AgentResult with the given text response.
func agentResultWithText(text string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: text},
			},
		},
	}
}

func TestRunSubAgent(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("happy path", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			assert.Equal(t, "do something", call.Prompt)
			assert.Equal(t, int64(4096), call.MaxOutputTokens)
			return agentResultWithText("done"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "do something",
			SessionTitle:   "Test Session",
		})
		require.NoError(t, err)
		assert.Equal(t, "done", resp.Content)
		assert.False(t, resp.IsError)
	})

	t.Run("cost update failure preserves output", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("output before cost failure"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      "missing-parent-session",
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "output before cost failure", resp.Content)
	})

	t.Run("response with text returns it", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("the answer"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "the answer", resp.Content)
	})

	t.Run("nil result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("empty result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("ModelCfg.MaxTokens overrides default", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := &mockSessionAgent{
			model: Model{
				CatwalkCfg: catwalk.Model{
					DefaultMaxTokens: 4096,
				},
				ModelCfg: config.SelectedModel{
					Provider:  providerID,
					MaxTokens: 8192,
				},
			},
			runFunc: func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
				assert.Equal(t, int64(8192), call.MaxOutputTokens)
				return agentResultWithText("ok"), nil
			},
		}

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Content)
	})

	t.Run("session creation failure with canceled context", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, nil)

		// Use a canceled context to trigger CreateTaskSession failure.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = coord.runSubAgent(ctx, subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
	})

	t.Run("provider not configured", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		// Agent references a provider that doesn't exist in config.
		agent := newMockAgent("unknown-provider", 4096, nil)

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "model provider not configured")
	})

	t.Run("agent run error returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, errors.New("provider request failed")
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		// runSubAgent returns (errorResponse, nil) when agent.Run fails — not a Go error.
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		// The error now also carries the session ID and a resume hint so
		// the caller can continue instead of starting over (see
		// TestRunSubAgent_Resume).
		assert.Contains(t, resp.Content, "Failed to generate response: provider request failed")
		assert.Contains(t, resp.Content, `resume_session_id="msg-1$$call-1"`)
	})

	t.Run("session setup callback is invoked", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var setupCalledWith string
		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
			SessionSetup: func(sessionID string) {
				setupCalledWith = sessionID
			},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, setupCalledWith, "SessionSetup should have been called")
	})

	t.Run("cost propagation to parent session", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			// Simulate the agent incurring cost by updating the child session.
			childSession, err := env.sessions.Get(ctx, call.SessionID)
			if err != nil {
				return nil, err
			}
			childSession.Cost = 0.05
			_, err = env.sessions.Save(ctx, childSession)
			if err != nil {
				return nil, err
			}
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.05, updated.Cost, 1e-9)
	})
}

// TestRunSubAgent_Background verifies that Background:true detaches the
// sub-agent immediately: runSubAgent must return before the underlying
// agent run finishes, with a response describing the backgrounded
// session, and the sub-agent registry must already show it as running.
// The eventual completion path (completeSubAgentBackgrounded queuing a
// follow-up message via coordinator.Run) is exercised by the same,
// pre-existing machinery the Ctrl+B background trigger already uses --
// it is intentionally not driven to completion here, since that would
// require a fully wired coordinator (see agenttest.NewCoordinator)
// rather than this package's minimal unit-test coordinator.
func TestRunSubAgent_Background(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, providerCfg)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	// release is deliberately never closed: the mock agent's run stays
	// blocked for the lifetime of the test so it can never reach
	// completeSubAgentBackgrounded, which would otherwise attempt to
	// queue a follow-up through this test's incompletely-wired
	// coordinator.
	release := make(chan struct{})
	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		<-release
		return agentResultWithText("background done"), nil
	})

	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parentSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "test",
		SessionTitle:   "Test",
		ToolName:       AgentToolName,
		Background:     true,
	})
	require.NoError(t, err)
	assert.False(t, resp.IsError)
	assert.Contains(t, resp.Content, "background")

	// The tool call must return before the agent's run has even
	// finished -- the mock agent is still blocked on release.
	statuses := coord.subAgents.list(parentSession.ID)
	require.Len(t, statuses, 1)
	assert.Equal(t, SubAgentRunning, statuses[0].State)
}

func TestRunSubAgent_Resume(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("continues an existing session", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		coord.taskAgents = csync.NewMap[string, SessionAgent]()

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		childSession, err := env.sessions.CreateTaskSession(t.Context(), "existing-child", parentSession.ID, "Original Task")
		require.NoError(t, err)

		var sawSessionID string
		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			sawSessionID = call.SessionID
			assert.Equal(t, "keep going", call.Prompt)
			return agentResultWithText("continued"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-2",
			ToolCallID:      "call-2",
			Prompt:          "keep going",
			SessionTitle:    "Ignored On Resume",
			ResumeSessionID: childSession.ID,
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "continued", resp.Content)
		// The run continued the existing session, not a new one derived
		// from AgentMessageID/ToolCallID.
		assert.Equal(t, childSession.ID, sawSessionID)
	})

	t.Run("rejects an unknown session", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		coord.taskAgents = csync.NewMap[string, SessionAgent]()

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			t.Fatal("should not run: resume target does not exist")
			return nil, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			Prompt:          "keep going",
			ResumeSessionID: "does-not-exist",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Contains(t, resp.Content, "not found")
	})

	t.Run("rejects a session from another conversation", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		coord.taskAgents = csync.NewMap[string, SessionAgent]()

		ownerSession, err := env.sessions.Create(t.Context(), "Owner")
		require.NoError(t, err)
		otherSession, err := env.sessions.Create(t.Context(), "Unrelated")
		require.NoError(t, err)
		childOfOther, err := env.sessions.CreateTaskSession(t.Context(), "child-of-other", otherSession.ID, "Task")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			t.Fatal("should not run: resume target belongs to a different conversation")
			return nil, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       ownerSession.ID,
			Prompt:          "keep going",
			ResumeSessionID: childOfOther.ID,
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Contains(t, resp.Content, "does not belong")
	})

	t.Run("rejects a session that is still running", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		childSession, err := env.sessions.CreateTaskSession(t.Context(), "busy-child", parentSession.ID, "Task")
		require.NoError(t, err)

		busyAgent := newMockAgent(providerID, 4096, nil)
		busyAgent.busySessionID = childSession.ID
		coord.taskAgents = csync.NewMap[string, SessionAgent]()
		coord.taskAgents.Set("busy-key", busyAgent)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			t.Fatal("should not run: resume target is still busy")
			return nil, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			Prompt:          "keep going",
			ResumeSessionID: childSession.ID,
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Contains(t, resp.Content, "still running")
	})
}

func TestUpdateParentSessionCost(t *testing.T) {
	t.Run("accumulates cost correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		// Set child cost.
		child.Cost = 0.10
		_, err = env.sessions.Save(t.Context(), child)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.10, updated.Cost, 1e-9)
	})

	t.Run("accumulates multiple child costs", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child1, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child1")
		require.NoError(t, err)
		child1.Cost = 0.05
		_, err = env.sessions.Save(t.Context(), child1)
		require.NoError(t, err)

		child2, err := env.sessions.CreateTaskSession(t.Context(), "tool-2", parent.ID, "Child2")
		require.NoError(t, err)
		child2.Cost = 0.03
		_, err = env.sessions.Save(t.Context(), child2)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child1.ID, parent.ID)
		require.NoError(t, err)
		err = coord.updateParentSessionCost(t.Context(), child2.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.08, updated.Cost, 1e-9)
	})

	t.Run("child session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), "non-existent", parent.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get child session")
	})

	t.Run("parent session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, "non-existent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get parent session")
	})

	t.Run("zero cost handled correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.0, updated.Cost, 1e-9)
	})
}

func TestGetProviderOptionsReasoningEffort(t *testing.T) {
	// Bedrock is Fantasy's Anthropic under a different provider name; options
	// must land under anthropic.Name so the Anthropic language model picks them up.
	tests := []struct {
		name         string
		providerType catwalk.Type
	}{
		{"anthropic honors reasoning_effort", catwalk.Type(anthropic.Name)},
		{"bedrock honors reasoning_effort", catwalk.Type(bedrock.Name)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{
					ID:              "claude-opus-4-7",
					CanReason:       true,
					ReasoningLevels: []string{"max"},
				},
				ModelCfg: config.SelectedModel{
					Provider:        "test",
					ReasoningEffort: "max",
				},
			}
			providerCfg := config.ProviderConfig{ID: "test", Type: tc.providerType}

			opts := getProviderOptions(model, providerCfg)

			raw, ok := opts[anthropic.Name]
			require.True(t, ok, "options should be keyed under anthropic.Name for type %q", tc.providerType)
			parsed, ok := raw.(*anthropic.ProviderOptions)
			require.True(t, ok)
			require.NotNil(t, parsed.Effort)
			assert.Equal(t, anthropic.Effort("max"), *parsed.Effort)
		})
	}
}

func TestGetProviderOptionsThinkingBudget(t *testing.T) {
	// Direct Anthropic/Bedrock models have no catwalk ReasoningLevels
	// (Anthropic's API takes a numeric budget_tokens, not named
	// levels), so the off/low/medium/high picker is Crush's own and
	// resolved via config.ThinkingBudgetTokens instead of the
	// catwalk-driven "effort" path.
	tests := []struct {
		name            string
		reasoningEffort string
		think           bool
		wantBudget      int64 // 0 means no thinking option should be set
	}{
		{"low level sets its budget", "low", false, config.ThinkingBudgetTokens("low")},
		{"medium level sets its budget", "medium", false, config.ThinkingBudgetTokens("medium")},
		{"high level sets its budget", "high", false, config.ThinkingBudgetTokens("high")},
		{"off level disables thinking regardless of legacy Think", "off", true, 0},
		{"legacy Think alone falls back to the old fixed budget", "", true, 2000},
		{"neither level nor Think set disables thinking", "", false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{
					ID:        "claude-opus-4-7",
					CanReason: true,
				},
				ModelCfg: config.SelectedModel{
					Provider:        "test",
					ReasoningEffort: tc.reasoningEffort,
					Think:           tc.think,
				},
			}
			providerCfg := config.ProviderConfig{ID: "test", Type: catwalk.Type(anthropic.Name)}

			opts := getProviderOptions(model, providerCfg)

			raw, ok := opts[anthropic.Name]
			require.True(t, ok)
			parsed, ok := raw.(*anthropic.ProviderOptions)
			require.True(t, ok)

			if tc.wantBudget == 0 {
				assert.Nil(t, parsed.Thinking)
				return
			}
			require.NotNil(t, parsed.Thinking)
			assert.Equal(t, tc.wantBudget, parsed.Thinking.BudgetTokens)
		})
	}
}

func TestGetProviderOptionsGoogleThinking(t *testing.T) {
	tests := []struct {
		name            string
		modelID         string
		reasoningEffort string
		think           bool
		wantBudget      *int64 // nil means "not set"
		wantLevel       string // "" means "not set"
	}{
		{"gemini 2.x low sets a token budget", "gemini-2.5-flash", "low", false, ptr(config.ThinkingBudgetTokens("low")), ""},
		{"gemini 2.x off disables via a zero budget", "gemini-2.5-flash", "off", false, ptr(int64(0)), ""},
		{"gemini 2.x legacy Think falls back to a low budget", "gemini-2.5-flash", "", true, ptr(config.ThinkingBudgetTokens("low")), ""},
		{"gemini 2.x neither level nor Think sets nothing", "gemini-2.5-flash", "", false, nil, ""},
		{"gemini 3+ low maps to the uppercase level enum", "gemini-3-pro", "low", false, nil, google.ThinkingLevelLow},
		{"gemini 3+ medium maps to the uppercase level enum", "gemini-3-pro", "medium", false, nil, google.ThinkingLevelMedium},
		{"gemini 3+ high maps to the uppercase level enum", "gemini-3-pro", "high", false, nil, google.ThinkingLevelHigh},
		{"gemini 3+ off has no disabling equivalent, so nothing is set", "gemini-3-pro", "off", false, nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{
					ID:        tc.modelID,
					CanReason: true,
				},
				ModelCfg: config.SelectedModel{
					Provider:        "test",
					ReasoningEffort: tc.reasoningEffort,
					Think:           tc.think,
				},
			}
			providerCfg := config.ProviderConfig{ID: "test", Type: catwalk.Type(google.Name)}

			opts := getProviderOptions(model, providerCfg)

			raw, ok := opts[google.Name]
			require.True(t, ok)
			parsed, ok := raw.(*google.ProviderOptions)
			require.True(t, ok)

			if tc.wantBudget == nil && tc.wantLevel == "" {
				assert.Nil(t, parsed.ThinkingConfig)
				return
			}
			require.NotNil(t, parsed.ThinkingConfig)
			if tc.wantBudget != nil {
				require.NotNil(t, parsed.ThinkingConfig.ThinkingBudget)
				assert.Equal(t, *tc.wantBudget, *parsed.ThinkingConfig.ThinkingBudget)
			}
			if tc.wantLevel != "" {
				require.NotNil(t, parsed.ThinkingConfig.ThinkingLevel)
				assert.Equal(t, tc.wantLevel, *parsed.ThinkingConfig.ThinkingLevel)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestBuildGeminiCliProviderDoesNotRequireAPIKey is a regression test for
// a bug where selecting any Gemini CLI or Antigravity model always failed
// silently: genai.NewClient hard-requires a non-empty APIKey for any
// non-Vertex backend even when Credentials/skipAuth are set, so
// buildGeminiCliProvider's use of google.WithSkipAuth(true) alone (with
// no API key) made every model switch to these providers fail with "api
// key is required for Google AI backend" as soon as the language model
// was constructed -- which looked to the user like the picker selection
// silently not taking effect, since the resulting error just failed the
// background UpdateAgentModel call.
func TestBuildGeminiCliProviderDoesNotRequireAPIKey(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		identity   geminicli.Identity
	}{
		{"gemini cli", geminicli.ProviderID, geminicli.GeminiCLIIdentity},
		{"antigravity", antigravity.ProviderID, antigravity.Identity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			providerCfg := config.ProviderConfig{
				ID:      tc.providerID,
				Type:    catwalk.TypeGoogle,
				BaseURL: geminicli.BaseURL,
				APIKey:  "fake-access-token",
				Models: []catwalk.Model{
					{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro", CanReason: true, ContextWindow: 2097152, DefaultMaxTokens: 65536},
				},
				OAuthExtra: map[string]string{"project_id": "proj-1"},
			}
			coord := newTestCoordinator(t, env, tc.providerID, providerCfg)

			modelCfg := config.SelectedModel{
				Provider: tc.providerID,
				Model:    "gemini-2.5-pro",
			}

			m, err := coord.buildModelFromSelected(context.Background(), modelCfg, false)
			require.NoError(t, err)
			require.Equal(t, "gemini-2.5-pro", m.CatwalkCfg.ID)
		})
	}
}

func TestGetProviderOptionsReasoningEffortFallback(t *testing.T) {
	model := Model{
		CatwalkCfg: catwalk.Model{
			ID:              "glm-5.2",
			CanReason:       true,
			ReasoningLevels: []string{"high", "max"},
		},
		ModelCfg: config.SelectedModel{
			Provider: "zai",
		},
	}
	providerCfg := config.ProviderConfig{
		ID:   string(catwalk.InferenceProviderZAI),
		Type: openaicompat.Name,
	}

	opts := getProviderOptions(model, providerCfg)

	raw, ok := opts[openaicompat.Name]
	require.True(t, ok)
	parsed, ok := raw.(*openaicompat.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, parsed.ReasoningEffort)
	assert.Equal(t, "high", string(*parsed.ReasoningEffort))

	thinking, ok := parsed.ExtraBody["thinking"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "enabled", thinking["type"])
}

func TestSkillsToDeactivate(t *testing.T) {
	t.Parallel()

	active := []string{"caveman", "jq"}
	tests := []struct {
		name   string
		prompt string
		want   []string
	}{
		{"no match", "how do I query Loki?", nil},
		{"stop one", "stop caveman please", []string{"caveman"}},
		{"disable one", "disable jq", []string{"jq"}},
		{"turn off one", "turn off caveman now", []string{"caveman"}},
		{"unload one", "unload jq", []string{"jq"}},
		{"normal mode clears all", "back to normal mode", []string{"caveman", "jq"}},
		{"stop all skills", "stop all skills", []string{"caveman", "jq"}},
		{"case insensitive", "STOP Caveman", []string{"caveman"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, skillsToDeactivate(tt.prompt, active))
		})
	}
}
