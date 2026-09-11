package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// recordingStreamModel streams a normal text response but first records the
// full Prompt (system/user/assistant history) the caller built for this
// step, so a test can inspect exactly what would be sent to the provider.
type recordingStreamModel struct {
	text    string
	prompts []fantasy.Prompt
}

func (m *recordingStreamModel) Provider() string { return "fake" }
func (m *recordingStreamModel) Model() string    { return "fake-model" }

func (m *recordingStreamModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *recordingStreamModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.prompts = append(m.prompts, call.Prompt)
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

func (m *recordingStreamModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *recordingStreamModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestActiveSkillsInjection_UsesUserRoleNotSystem is the regression guard
// for a bug where an active skill's instructions were silently dropped on
// Anthropic models. Anthropic's provider only honors a single system block
// at the very start of the prompt; any later fantasy.MessageRoleSystem
// message -- separated from the first by user/assistant turns, which is
// exactly the shape of the skills injection appended mid-conversation on
// every turn -- is silently discarded before the request is even sent.
// The injection must therefore ride as a user message, matching the
// working pattern already used for long-term memory recall.
func TestActiveSkillsInjection_UsesUserRoleNotSystem(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	large := &recordingStreamModel{text: "done"}
	small := &finishStreamModel{text: "title"}

	const skillBlock = "<active_skills>...caveman...</active_skills>"

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{Model: large, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SmallModel: Model{Model: small, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:     true,
		Sessions:   env.sessions,
		Messages:   env.messages,
		ActiveSkillsFor: func(sessionID string) string {
			return skillBlock
		},
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	require.NoError(t, err)

	require.NotEmpty(t, large.prompts, "the model must have been called at least once")
	prompt := large.prompts[len(large.prompts)-1]

	var found bool
	for _, msg := range prompt {
		for _, part := range msg.Content {
			text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
			if !ok || text.Text != skillBlock {
				continue
			}
			found = true
			require.Equal(t, fantasy.MessageRoleUser, msg.Role,
				"the skills block must ride as a user message: Anthropic silently drops "+
					"a system message that follows the conversation, which would make an "+
					"active skill's instructions a silent no-op")
		}
	}
	require.True(t, found, "the active-skills block must appear in the prompt sent to the model")
}
