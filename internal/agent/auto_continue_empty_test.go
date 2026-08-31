package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// emptyThenTextStreamModel streams a genuinely empty response (a clean
// FinishReasonStop with no text, tool calls, or reasoning) on its first N
// calls, then a normal text response on the call after that. It stands in
// for a model like a flaky preview/stealth model that occasionally abandons
// a turn with nothing instead of replying or erroring.
type emptyThenTextStreamModel struct {
	text       string
	emptyCalls int64
	calls      atomic.Int64
}

func (m *emptyThenTextStreamModel) Provider() string { return "fake" }
func (m *emptyThenTextStreamModel) Model() string    { return "fake-model" }

func (m *emptyThenTextStreamModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *emptyThenTextStreamModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	n := m.calls.Add(1)
	if n <= m.emptyCalls {
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}
	text := m.text
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *emptyThenTextStreamModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *emptyThenTextStreamModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// newAutoContinueTestAgent builds a sessionAgent whose large model has
// AutoContinueOnEmpty set as requested, backed by large/small fake models
// that need no network or recorded cassette.
func newAutoContinueTestAgent(t *testing.T, large fantasy.LanguageModel, autoContinue bool) (*sessionAgent, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	small := &finishStreamModel{text: "title"}
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			Model:      large,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
			ModelCfg:   config.SelectedModel{AutoContinueOnEmpty: autoContinue},
		},
		SmallModel: Model{Model: small, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:     true,
		Sessions:   env.sessions,
		Messages:   env.messages,
	}).(*sessionAgent)
	return sa, env
}

// TestAutoContinueOnEmpty_ResubmitsAfterEmptyResponse proves the fix: a model
// opted into AutoContinueOnEmpty that stops with a genuinely empty response
// is automatically re-prompted to continue, and the turn still lands the
// real answer instead of leaving the user with a silent, empty message.
func TestAutoContinueOnEmpty_ResubmitsAfterEmptyResponse(t *testing.T) {
	t.Parallel()

	model := &emptyThenTextStreamModel{text: "done", emptyCalls: 1}
	sa, env := newAutoContinueTestAgent(t, model, true)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	res, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistants []message.Message
	var continuedUser bool
	for _, m := range msgs {
		switch m.Role {
		case message.Assistant:
			assistants = append(assistants, m)
		case message.User:
			if m.Content().String() == "Your previous turn ended with an empty response. Continue where you left off." {
				continuedUser = true
			}
		}
	}
	require.True(t, continuedUser, "the auto-continue must appear as its own synthetic user turn")
	require.Len(t, assistants, 2, "the empty step and the recovered reply are two distinct assistant messages")

	first := assistants[0]
	require.Equal(t, message.FinishReasonError, first.FinishReason(), "the empty step is surfaced, not hidden")
	require.Contains(t, first.FinishPart().Details, "auto-continuing")

	last := assistants[1]
	require.Equal(t, "done", last.Content().Text, "the auto-continued turn carries the real reply")
	require.Equal(t, message.FinishReasonEndTurn, last.FinishReason())
}

// TestAutoContinueOnEmpty_GivesUpAfterMaxAttempts proves the retry is
// bounded: a model that never recovers stops auto-continuing after
// maxEmptyResponseAutoContinues attempts instead of looping forever.
func TestAutoContinueOnEmpty_GivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	model := &emptyThenTextStreamModel{text: "unused", emptyCalls: 1000}
	sa, env := newAutoContinueTestAgent(t, model, true)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistants int
	var gaveUp bool
	for _, m := range msgs {
		if m.Role == message.Assistant {
			assistants++
			if m.FinishReason() == message.FinishReasonError {
				if fp := m.FinishPart(); fp != nil && fp.Details != "" &&
					fp.Details == "The model returned no content after 3 auto-continue attempt(s); giving up." {
					gaveUp = true
				}
			}
		}
	}
	require.Equal(t, maxEmptyResponseAutoContinues+1, assistants,
		"the original empty step plus one per auto-continue attempt, then it must stop")
	require.True(t, gaveUp, "the final attempt must explain that it gave up")
}

// TestAutoContinueOnEmpty_DisabledLeavesEmptyEndTurnAlone is the regression
// guard: a model that has not opted in must see no behavior change when it
// ends a turn with a recognized "stop" and no output -- that is treated as
// a legitimate silent finish, exactly as before this feature existed.
func TestAutoContinueOnEmpty_DisabledLeavesEmptyEndTurnAlone(t *testing.T) {
	t.Parallel()

	model := &emptyThenTextStreamModel{text: "unused", emptyCalls: 1}
	sa, env := newAutoContinueTestAgent(t, model, false)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistants int
	for _, m := range msgs {
		if m.Role == message.Assistant {
			assistants++
			require.Equal(t, message.FinishReasonEndTurn, m.FinishReason(),
				"without opt-in, an empty stop stays a plain end-of-turn, not an error")
		}
	}
	require.Equal(t, 1, assistants, "no auto-continue must be queued when the option is off")
}

// switchingStreamModel streams a single empty response and, in the process,
// invokes a callback -- used to simulate the user switching the large model
// in the moment between an empty response being detected and its queued
// auto-continue actually running.
type switchingStreamModel struct {
	onStream func()
}

func (m *switchingStreamModel) Provider() string { return "fake" }
func (m *switchingStreamModel) Model() string    { return "fake-model-a" }

func (m *switchingStreamModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *switchingStreamModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if cb := m.onStream; cb != nil {
		m.onStream = nil
		cb()
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *switchingStreamModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *switchingStreamModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestAutoContinueOnEmpty_DropsQueuedContinuationAfterModelSwitch is the
// regression guard for the bug where a queued auto-continue prompt -- built
// to recover model A's empty response -- fired against model B after the
// user switched the large-model slot while it sat queued. The prompt must
// be dropped instead of being handed to a model that never produced the
// empty response it references.
func TestAutoContinueOnEmpty_DropsQueuedContinuationAfterModelSwitch(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	small := &finishStreamModel{text: "title"}
	modelB := &finishStreamModel{text: "from-b"}

	modelA := &switchingStreamModel{}
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			Model:      modelA,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
			ModelCfg:   config.SelectedModel{Provider: "provider-a", Model: "model-a", AutoContinueOnEmpty: true},
		},
		SmallModel: Model{Model: small, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:     true,
		Sessions:   env.sessions,
		Messages:   env.messages,
	}).(*sessionAgent)

	// Mid-flight, as if the user switched models in the picker: swap the
	// large model to B (no AutoContinueOnEmpty, different provider/model
	// ids) before the queued continuation gets a chance to run.
	modelA.onStream = func() {
		sa.SetModels(Model{
			Model:      modelB,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
			ModelCfg:   config.SelectedModel{Provider: "provider-b", Model: "model-b"},
		}, Model{Model: small, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}})
	}

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistants int
	var sawContinuePrompt bool
	for _, m := range msgs {
		switch m.Role {
		case message.Assistant:
			assistants++
			require.Equal(t, "model-a", m.Model, "model B must never be asked to answer model A's empty response")
		case message.User:
			if m.Content().String() == "Your previous turn ended with an empty response. Continue where you left off." {
				sawContinuePrompt = true
			}
		}
	}
	require.Equal(t, 1, assistants, "the stale continuation must be dropped, not answered by the new model")
	require.False(t, sawContinuePrompt, "a dropped continuation must not even appear as a synthetic user turn")
}
