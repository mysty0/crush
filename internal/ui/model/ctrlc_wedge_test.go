package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/ui/dialog"
)

// ctrlCWorkspace extends the shared countingWorkspace stub with the
// session-scoped busy probe and the keep-queue cancel used by the Ctrl+C
// path. busySessions is keyed by session ID so a test can leave a foreign
// session busy (a sub-agent child session, say) while the session the user
// is looking at is idle.
type ctrlCWorkspace struct {
	*countingWorkspace

	busySessions         map[string]bool
	sessionBusyCalls     int
	canceled             []string
	cancelKeepQueueCalls int
}

func (w *ctrlCWorkspace) AgentIsSessionBusy(sessionID string) bool {
	w.sessionBusyCalls++
	return w.busySessions[sessionID]
}

func (w *ctrlCWorkspace) AgentCancel(sessionID string) {
	w.cancelCalls++
	w.canceled = append(w.canceled, sessionID)
}

func (w *ctrlCWorkspace) AgentCancelKeepQueue(sessionID string) {
	w.cancelKeepQueueCalls++
	w.cancelCalls++
	w.canceled = append(w.canceled, sessionID)
}

// newCtrlCUI builds a UI on session "s1" whose memoized global busy state
// is true — some session somewhere is running — while busySessions decides
// which sessions are actually busy.
func newCtrlCUI(t *testing.T, busySessions map[string]bool) (*UI, *ctrlCWorkspace) {
	t.Helper()
	base := &countingWorkspace{ready: true, agentBusy: true}
	ws := &ctrlCWorkspace{countingWorkspace: base, busySessions: busySessions}
	m := newBusyUI(base)
	m.com.Workspace = ws
	warmCaches(m, true)
	return m, ws
}

func ctrlCKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
}

// TestCtrlCIgnoresForeignBusySession pins the session-scoping fix: the
// coordinator's busy map is global, so a sub-agent child session (the
// "...$$toolu_..." key) or any other session left running kept the cached
// global busy state pinned true forever. Ctrl+C then always took the
// "cancel the current turn" path — cancelling a session that was already
// idle — and the quit dialog became unreachable. The gate must look at the
// session the user is actually looking at.
func TestCtrlCIgnoresForeignBusySession(t *testing.T) {
	pinTTLs(t)

	m, ws := newCtrlCUI(t, map[string]bool{
		"4ea6182e-1ddf-4ca6-8261-f840358b3040$$toolu_01MqvZj6Kn": true,
	})

	m.Update(ctrlCKey())

	require.True(t, m.isAgentBusy(),
		"the global busy state stays true: a foreign session is running")
	require.Zero(t, ws.cancelCalls,
		"Ctrl+C must not cancel an idle session because another one is busy")
	require.True(t, m.dialog.ContainsDialog(dialog.QuitID),
		"Ctrl+C must reach the quit dialog when the current session is idle")
}

// TestCtrlCTwiceWhileBusyOpensQuitDialog pins the escape hatch: the first
// press cancels the current session's turn, and if that press did not make
// the session idle, the next one must open the quit dialog. Two presses
// always get the user out, whatever state the agent machinery is in.
func TestCtrlCTwiceWhileBusyOpensQuitDialog(t *testing.T) {
	pinTTLs(t)

	m, ws := newCtrlCUI(t, map[string]bool{"s1": true})

	m.Update(ctrlCKey())
	require.Equal(t, []string{"s1"}, ws.canceled,
		"the first Ctrl+C must cancel the current session's turn")
	require.False(t, m.dialog.ContainsDialog(dialog.QuitID),
		"the first Ctrl+C must not open the quit dialog while busy")
	require.True(t, m.ctrlCArmed, "the first Ctrl+C must arm the escape hatch")

	m.Update(ctrlCKey())
	require.Equal(t, 1, ws.cancelCalls,
		"the second Ctrl+C must not cancel again")
	require.True(t, m.dialog.ContainsDialog(dialog.QuitID),
		"a second Ctrl+C on a still-busy session must open the quit dialog")
	require.False(t, m.ctrlCArmed, "opening the quit dialog must disarm")
}

// TestCtrlCArmDisarmsOnOtherKeys guards against a stale arm: once the user
// presses anything else, the next Ctrl+C must cancel again rather than
// surprising them with a quit dialog.
func TestCtrlCArmDisarmsOnOtherKeys(t *testing.T) {
	pinTTLs(t)

	m, ws := newCtrlCUI(t, map[string]bool{"s1": true})

	m.Update(ctrlCKey())
	require.True(t, m.ctrlCArmed)

	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	require.False(t, m.ctrlCArmed, "any other key must disarm the escape hatch")

	m.Update(ctrlCKey())
	require.Equal(t, 2, ws.cancelCalls,
		"after disarming, Ctrl+C must cancel the turn again")
	require.False(t, m.dialog.ContainsDialog(dialog.QuitID),
		"a non-consecutive Ctrl+C must not open the quit dialog while busy")
}

// TestCtrlCClearsUnsentTextWhenIdle keeps the existing unsent-text branch
// intact: with the current session idle, the first Ctrl+C clears the
// prompt and only the next one opens the quit dialog.
func TestCtrlCClearsUnsentTextWhenIdle(t *testing.T) {
	pinTTLs(t)

	m, ws := newCtrlCUI(t, nil)
	m.textarea.SetValue("unsent prompt")

	m.Update(ctrlCKey())
	require.Empty(t, m.textarea.Value(), "the first Ctrl+C must clear the prompt")
	require.Zero(t, ws.cancelCalls)
	require.False(t, m.dialog.ContainsDialog(dialog.QuitID),
		"clearing the prompt must not open the quit dialog")

	m.Update(ctrlCKey())
	require.True(t, m.dialog.ContainsDialog(dialog.QuitID),
		"Ctrl+C on an empty prompt must open the quit dialog")
}

// TestCtrlCDecisionProbesSessionOnce pins the cost of the session-scoped
// gate: it is a synchronous workspace call (an HTTP round-trip in
// client/server mode), so it must happen once per Ctrl+C press and never
// leak into other message traffic.
func TestCtrlCDecisionProbesSessionOnce(t *testing.T) {
	pinTTLs(t)

	m, ws := newCtrlCUI(t, map[string]bool{"s1": true})

	for range 25 {
		m.Update(plainMsg{})
	}
	require.Zero(t, ws.sessionBusyCalls,
		"ordinary messages must not probe the session-scoped busy state")

	m.Update(ctrlCKey())
	require.Equal(t, 1, ws.sessionBusyCalls,
		"one Ctrl+C press must probe the session exactly once")
}

// TestCtrlCCancelDiscardsQueue pins the fix for a real report: canceling a
// busy turn with Ctrl+C used to call AgentCancelKeepQueue and leave queued
// follow-up prompts in place, so they silently ran as the next turn right
// after the user asked to stop. Ctrl+C must discard the queue exactly like
// Esc does, and the "N Queued" pill must reflect that immediately.
func TestCtrlCCancelDiscardsQueue(t *testing.T) {
	pinTTLs(t)

	m, ws := newCtrlCUI(t, map[string]bool{"s1": true})
	m.promptQueue = 2
	m.promptQueueItems = []string{"a", "b"}

	m.Update(ctrlCKey())

	require.Equal(t, []string{"s1"}, ws.canceled,
		"Ctrl+C must cancel the current session's turn")
	require.Zero(t, ws.cancelKeepQueueCalls,
		"Ctrl+C must not use the keep-queue cancel path")
	require.Zero(t, m.promptQueue,
		"the queue pill must clear immediately, not wait for a refresh")
	require.Empty(t, m.promptQueueItems)
}
