package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestCancelKeepQueue_SurvivesCancelMark is the regression for a canceled
// turn silently taking its queued follow-up with it.
//
// Cancel records a cancel mark covering every accept sequence issued so
// far, so one Escape also covers prompts still in the scheduler window.
// A prompt queued *before* the cancel has a sequence at or below that
// mark too, so the drain sites (drainQueueForStep and the post-turn
// handoff in Run) dropped it -- CancelKeepQueue kept the queue only until
// the next drain looked at it, then discarded it anyway. Escape with a
// message queued therefore stopped the turn and threw the follow-up away
// instead of running it.
//
// The mark is only recorded while an accepted run is in flight, which is
// why this test holds one: without it the mark is never set and the bug
// cannot reproduce.
func TestCancelKeepQueue_SurvivesCancelMark(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	broker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(broker.Shutdown)

	a := NewSessionAgent(SessionAgentOptions{
		Sessions:    env.sessions,
		Messages:    env.messages,
		RunComplete: broker,
	}).(*sessionAgent)

	const sessionID = "keep-queue-vs-mark"

	// An in-flight accepted run: this is what makes Cancel record a mark.
	accepted := a.BeginAccepted(sessionID)
	t.Cleanup(accepted.Close)

	// A follow-up queued while that run is active, stamped with the
	// accept sequence the real queue path assigns.
	queuedAccept := a.BeginAccepted(sessionID)
	a.messageQueue.Set(sessionID, []SessionAgentCall{
		{SessionID: sessionID, Prompt: "follow-up", acceptSeq: queuedAccept.seq},
	})

	a.CancelKeepQueue(sessionID)

	got, ok := a.messageQueue.Get(sessionID)
	require.True(t, ok, "CancelKeepQueue must not clear the queue")
	require.Len(t, got, 1)
	require.Equal(t, "follow-up", got[0].Prompt)

	// The real assertion: the queued prompt must not be covered by the
	// cancel mark, or the next drain discards it.
	require.False(t, a.canceledBySeq(sessionID, got[0].acceptSeq),
		"a kept queued prompt must not be covered by the cancel mark")

	// And it must survive the production drain unchanged.
	fold, canceledWithRunID := a.drainQueueForStep(sessionID)
	require.Empty(t, canceledWithRunID, "the kept prompt must not be reported as canceled")
	require.Len(t, fold, 1, "the kept prompt must be folded into the next turn")
	require.Equal(t, "follow-up", fold[0].Prompt)
}

// TestCancel_StillDiscardsQueue confirms the plain Cancel path is
// unchanged: it must still clear queued prompts, so the fix above did not
// turn every cancel into a keep-queue cancel.
func TestCancel_StillDiscardsQueue(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	broker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(broker.Shutdown)

	a := NewSessionAgent(SessionAgentOptions{
		Sessions:    env.sessions,
		Messages:    env.messages,
		RunComplete: broker,
	}).(*sessionAgent)

	const sessionID = "cancel-discards"
	accepted := a.BeginAccepted(sessionID)
	t.Cleanup(accepted.Close)

	queuedAccept := a.BeginAccepted(sessionID)
	a.messageQueue.Set(sessionID, []SessionAgentCall{
		{SessionID: sessionID, Prompt: "discard me", acceptSeq: queuedAccept.seq},
	})

	a.Cancel(sessionID)

	require.Zero(t, a.QueuedPrompts(sessionID),
		"Cancel must still discard queued prompts")
}
