package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// rateLimitedThenTextStreamModel fails its first N Stream() calls with a
// retryable 429 (a near-zero Retry-After header so fantasy's own built-in
// backoff does not slow the test down), then streams normal text
// thereafter. It stands in for a provider whose rate limit clears up after
// a while.
type rateLimitedThenTextStreamModel struct {
	text      string
	failCalls int64
	calls     atomic.Int64
}

func (m *rateLimitedThenTextStreamModel) Provider() string { return "fake" }
func (m *rateLimitedThenTextStreamModel) Model() string    { return "fake-model" }

func (m *rateLimitedThenTextStreamModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *rateLimitedThenTextStreamModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.calls.Add(1) <= m.failCalls {
		return nil, &fantasy.ProviderError{
			Title:           "too many requests",
			Message:         "Provider returned error",
			StatusCode:      http.StatusTooManyRequests,
			ResponseHeaders: map[string]string{"retry-after-ms": "1"},
		}
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

func (m *rateLimitedThenTextStreamModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *rateLimitedThenTextStreamModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// withShortRateLimitCooldown replaces rateLimitCooldownSchedule with
// near-zero delays for the duration of a test, so auto-continue attempts
// don't actually wait tens of seconds to minutes. It mutates a package-level
// var, so callers must NOT run in parallel with each other (no t.Parallel()
// in this file) -- concurrent tests would race on the shared schedule.
func withShortRateLimitCooldown(t *testing.T) {
	t.Helper()
	orig := rateLimitCooldownSchedule
	rateLimitCooldownSchedule = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { rateLimitCooldownSchedule = orig })
}

// exhaustingCallsPerAttempt is how many underlying LanguageModel.Stream
// calls fantasy's own retry-with-backoff makes before giving up: the
// initial attempt plus providerMaxRetries retries.
const exhaustingCallsPerAttempt = providerMaxRetries + 1

func newAutoContinueRateLimitTestAgent(t *testing.T, large fantasy.LanguageModel, autoContinue bool) (*sessionAgent, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	small := &finishStreamModel{text: "title"}
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			Model:      large,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
			ModelCfg:   config.SelectedModel{Provider: "fake-provider", Model: "fake-model", AutoContinueOnRateLimit: autoContinue},
		},
		SmallModel: Model{Model: small, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:     true,
		Sessions:   env.sessions,
		Messages:   env.messages,
	}).(*sessionAgent)
	return sa, env
}

// TestAutoContinueOnRateLimit_RetriesAfterExhaustedBackoff proves the fix:
// once fantasy's own retry-with-backoff exhausts against a 429, a model
// opted into AutoContinueOnRateLimit gets one more attempt after a cooldown
// and recovers -- without a new visible user turn, since the retry resends
// the same request silently.
func TestAutoContinueOnRateLimit_RetriesAfterExhaustedBackoff(t *testing.T) {
	withShortRateLimitCooldown(t)

	model := &rateLimitedThenTextStreamModel{text: "done", failCalls: exhaustingCallsPerAttempt}
	sa, env := newAutoContinueRateLimitTestAgent(t, model, true)

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

	var users, assistants []message.Message
	for _, m := range msgs {
		switch m.Role {
		case message.User:
			users = append(users, m)
		case message.Assistant:
			assistants = append(assistants, m)
		}
	}
	require.Len(t, users, 1, "the silent retry must not add a new visible user turn")
	require.Equal(t, "hello", users[0].Content().String())

	require.Len(t, assistants, 2, "the exhausted attempt and the recovered reply are two distinct assistant messages")
	first := assistants[0]
	require.Equal(t, message.FinishReasonError, first.FinishReason())
	require.Contains(t, first.FinishPart().Details, "auto-continuing")

	last := assistants[1]
	require.Equal(t, "done", last.Content().Text, "the auto-continued turn carries the real reply")
	require.Equal(t, message.FinishReasonEndTurn, last.FinishReason())
}

// TestAutoContinueOnRateLimit_GivesUpAfterMaxAttempts proves the retry is
// bounded: a provider that never stops rate-limiting stops auto-continuing
// after maxRateLimitAutoContinues attempts instead of retrying forever.
func TestAutoContinueOnRateLimit_GivesUpAfterMaxAttempts(t *testing.T) {
	withShortRateLimitCooldown(t)

	model := &rateLimitedThenTextStreamModel{text: "unused", failCalls: 1_000_000}
	sa, env := newAutoContinueRateLimitTestAgent(t, model, true)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	require.Error(t, err, "a permanently rate-limited provider must eventually surface as a failed turn")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistants int
	var gaveUpWithoutRetryPromise bool
	for _, m := range msgs {
		if m.Role == message.Assistant {
			assistants++
			if m.FinishReason() == message.FinishReasonError {
				if fp := m.FinishPart(); fp != nil && fp.Details != "" && !strings.Contains(fp.Details, "auto-continuing") {
					gaveUpWithoutRetryPromise = true
				}
			}
		}
	}
	require.Equal(t, maxRateLimitAutoContinues+1, assistants,
		"the original exhausted attempt plus one per auto-continue attempt, then it must stop")
	require.True(t, gaveUpWithoutRetryPromise, "the final attempt must not promise a retry that will not happen")
}

// TestAutoContinueOnRateLimit_DisabledLeavesErrorAlone is the regression
// guard: a model that has not opted in must see no behavior change on an
// exhausted 429 -- it stays a single failed turn, exactly as before this
// feature existed.
func TestAutoContinueOnRateLimit_DisabledLeavesErrorAlone(t *testing.T) {
	withShortRateLimitCooldown(t)

	model := &rateLimitedThenTextStreamModel{text: "unused", failCalls: exhaustingCallsPerAttempt}
	sa, env := newAutoContinueRateLimitTestAgent(t, model, false)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	require.Error(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistants int
	for _, m := range msgs {
		if m.Role == message.Assistant {
			assistants++
			require.Equal(t, message.FinishReasonError, m.FinishReason())
		}
	}
	require.Equal(t, 1, assistants, "no auto-continue must be attempted when the option is off")
}
