package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routingTestProviderID is the stub provider the routing tests run every
// agent on. It is never contacted: the coder agent is a mock, and the
// sub-agents are mocks too.
const routingTestProviderID = "routing-test-provider"

// newRoutingTestCoordinator builds a coordinator wired far enough for
// coordinator.Run to actually reach currentAgent -- which is what these
// tests need to observe, since the bug being guarded against is
// precisely which session ID a backgrounded sub-agent's completion turn
// is handed to the coder agent under.
//
// It returns the coordinator plus a channel receiving every
// SessionAgentCall the coder agent is asked to run.
func newRoutingTestCoordinator(t *testing.T, env fakeEnv) (*coordinator, <-chan SessionAgentCall) {
	t.Helper()

	providerCfg := config.ProviderConfig{
		ID:      routingTestProviderID,
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:1/v1",
		APIKey:  "stub",
		Models:  []catwalk.Model{{ID: "routing-test-model", DefaultMaxTokens: 4096}},
	}
	coord := newTestCoordinator(t, env, routingTestProviderID, providerCfg)
	coord.permissions = env.permissions
	coord.taskAgents = csync.NewMap[string, SessionAgent]()
	coord.defaultTaskAgentKey = csync.NewValue[string]("")
	coord.loadedSkills = skills.NewLoadedStore()
	coord.cfg.Config().Models = map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: routingTestProviderID, Model: "routing-test-model"},
		config.SelectedModelTypeSmall: {Provider: routingTestProviderID, Model: "routing-test-model"},
	}

	coderRuns := make(chan SessionAgentCall, 8)
	coord.currentAgent = &mockSessionAgent{
		model: Model{
			CatwalkCfg: catwalk.Model{DefaultMaxTokens: 4096},
			ModelCfg:   config.SelectedModel{Provider: routingTestProviderID},
		},
		runFunc: func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			coderRuns <- call
			return agentResultWithText("ack"), nil
		},
	}
	return coord, coderRuns
}

// backgroundSubAgent asks for a backgrounded sub-agent from
// dispatchSessionID that immediately finishes with the given output.
// Whether the request is honored depends on the dispatching session:
// only a top-level conversation can receive a deferred completion.
func backgroundSubAgent(t *testing.T, coord *coordinator, dispatchSessionID, output string) {
	t.Helper()

	agent := newMockAgent(routingTestProviderID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText(output), nil
	})
	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      dispatchSessionID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "do something",
		SessionTitle:   "Backgrounded",
		ToolName:       AgentToolName,
		Background:     true,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
}

// awaitCall waits for a call on ch, failing the test if none arrives.
func awaitCall(t *testing.T, ch <-chan SessionAgentCall) SessionAgentCall {
	t.Helper()

	select {
	case call := <-ch:
		return call
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the backgrounded sub-agent's completion to be delivered")
		return SessionAgentCall{}
	}
}

// TestBackgroundedSubAgentCompletionRouting covers where a backgrounded
// sub-agent's completion turn is delivered once the sub-agent finishes.
//
// The invariant under test: the coder agent must only ever be asked to
// run top-level, user-facing sessions. Handing it a sub-agent session
// (an ID containing "$$") registers that session among its active
// requests, where no user-facing cancel can reach it -- the app then
// reports busy forever and Ctrl+C stops working.
func TestBackgroundedSubAgentCompletionRouting(t *testing.T) {
	t.Run("dispatched from a top-level session queues there", func(t *testing.T) {
		env := testEnv(t)
		coord, coderRuns := newRoutingTestCoordinator(t, env)

		top, err := env.sessions.Create(t.Context(), "Top level")
		require.NoError(t, err)

		backgroundSubAgent(t, coord, top.ID, "the answer")

		call := awaitCall(t, coderRuns)
		assert.Equal(t, top.ID, call.SessionID)
		assert.Contains(t, call.Prompt, "the answer")
	})

	// The two nested cases below drive deliverBackgroundedCompletion
	// directly rather than through a nested dispatch: runSubAgent no
	// longer backgrounds work requested by a sub-agent (see the last
	// subtest), so delivery is the only way those routes are reached.
	t.Run("a completion from a finished sub-agent session queues into its conversation", func(t *testing.T) {
		env := testEnv(t)
		coord, coderRuns := newRoutingTestCoordinator(t, env)

		top, err := env.sessions.Create(t.Context(), "Top level")
		require.NoError(t, err)
		nested := env.sessions.CreateAgentToolSessionID("msg-parent", "call-parent")
		_, err = env.sessions.CreateTaskSession(t.Context(), nested, top.ID, "Sub-agent")
		require.NoError(t, err)

		go coord.deliverBackgroundedCompletion(nested, "msg-x$$call-x", "the answer")

		call := awaitCall(t, coderRuns)
		assert.NotContains(t, call.SessionID, "$$",
			"the coder agent must never be handed a sub-agent session; that entry can never be canceled")
		assert.Equal(t, top.ID, call.SessionID)
		// The result must still be delivered, and must say where it
		// came from now that it is reported one level up.
		assert.Contains(t, call.Prompt, "the answer")
		assert.Contains(t, call.Prompt, nested)
	})

	t.Run("a completion from a running sub-agent session steers that sub-agent", func(t *testing.T) {
		env := testEnv(t)
		coord, coderRuns := newRoutingTestCoordinator(t, env)

		top, err := env.sessions.Create(t.Context(), "Top level")
		require.NoError(t, err)
		nested := env.sessions.CreateAgentToolSessionID("msg-parent", "call-parent")
		_, err = env.sessions.CreateTaskSession(t.Context(), nested, top.ID, "Sub-agent")
		require.NoError(t, err)

		steered := make(chan SessionAgentCall, 4)
		coord.taskAgents.Set("owner", &mockSessionAgent{
			busySessionID: nested,
			runFunc: func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
				steered <- call
				return agentResultWithText("ack"), nil
			},
		})

		go coord.deliverBackgroundedCompletion(nested, "msg-x$$call-x", "the answer")

		call := awaitCall(t, steered)
		assert.Equal(t, nested, call.SessionID)
		assert.Contains(t, call.Prompt, "the answer")

		// Delivery is done once the owning sub-agent took the
		// completion; nothing may reach the coder agent afterwards.
		time.Sleep(200 * time.Millisecond)
		assert.Empty(t, coderRuns, "a steered completion must not also start a coder turn")
	})

	// A sub-agent asking to background work is asking for a follow-up
	// it can never receive: its turn ending is what returns its answer.
	// Left alone, it would end that turn on "started, waiting for
	// results" and the real result would surface in the user's
	// conversation later, detached from the question that prompted it.
	t.Run("a sub-agent's own dispatch is never backgrounded", func(t *testing.T) {
		env := testEnv(t)
		coord, coderRuns := newRoutingTestCoordinator(t, env)

		top, err := env.sessions.Create(t.Context(), "Top level")
		require.NoError(t, err)
		nested := env.sessions.CreateAgentToolSessionID("msg-parent", "call-parent")
		_, err = env.sessions.CreateTaskSession(t.Context(), nested, top.ID, "Sub-agent")
		require.NoError(t, err)

		backgroundSubAgent(t, coord, nested, "the answer")

		time.Sleep(200 * time.Millisecond)
		assert.Empty(t, coderRuns,
			"the sub-agent got its result inline, so there is nothing to report later")
	})
}

// TestRouteBackgroundCompletion covers the routing decision itself,
// including the chains that end-to-end tests cannot easily build.
func TestRouteBackgroundCompletion(t *testing.T) {
	t.Parallel()

	t.Run("walks nested sub-agent sessions up to the conversation", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord := &coordinator{sessions: env.sessions}

		top, err := env.sessions.Create(t.Context(), "Top level")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "msg-a$$call-a", top.ID, "Sub-agent")
		require.NoError(t, err)
		grandchild, err := env.sessions.CreateTaskSession(t.Context(), "msg-b$$call-b", child.ID, "Nested sub-agent")
		require.NoError(t, err)

		route, err := coord.routeBackgroundCompletion(t.Context(), grandchild.ID)
		require.NoError(t, err)
		assert.Equal(t, top.ID, route.TopLevelSessionID)
		assert.True(t, route.Nested)
		assert.Empty(t, route.SteerSessionID, "no task agent is running the dispatching session")
	})

	t.Run("a top-level session routes to itself", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord := &coordinator{sessions: env.sessions}

		top, err := env.sessions.Create(t.Context(), "Top level")
		require.NoError(t, err)

		route, err := coord.routeBackgroundCompletion(t.Context(), top.ID)
		require.NoError(t, err)
		assert.Equal(t, top.ID, route.TopLevelSessionID)
		assert.False(t, route.Nested)
		assert.Empty(t, route.SteerSessionID)
	})

	t.Run("an orphaned parent chain is unresolvable", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord := &coordinator{sessions: env.sessions}

		orphan, err := env.sessions.CreateTaskSession(t.Context(), "msg-c$$call-c", "no-such-session", "Sub-agent")
		require.NoError(t, err)

		_, err = coord.routeBackgroundCompletion(t.Context(), orphan.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no-such-session")
	})
}
