package model

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/stretchr/testify/require"
)

// flapItem is a minimal, fully-controlled chat.MessageItem +
// chat.Animatable used to reproduce the exact stuck state recovered from
// the frozen live process: two Anim pointers cycling forever in
// Chat.Animate / Chat.RestartPausedVisibleAnimations, confirmed by
// attaching delve and tracing the sole event-loop goroutine.
//
// Unlike a real chat item, flapItem's rendered height is a directly
// settable field, so a test can force the exact boundary-crossing
// condition — an item's visibility flipping across the top of the
// viewport as a neighboring item's height changes — without depending
// on fragile pixel-accurate markdown/ANSI layout.
type flapItem struct {
	id     string
	height int
	ver    uint64

	startCalls int // counts every StartAnimation call, i.e. every new tea.Tick chain armed for this item.
}

func (f *flapItem) ID() string           { return f.id }
func (f *flapItem) Version() uint64      { return f.ver }
func (f *flapItem) Finished() bool       { return false } // never freezable: always "spinning".
func (f *flapItem) RawRender(int) string { return f.Render(0) }

func (f *flapItem) Render(int) string {
	s := ""
	for range f.height {
		s += "x\n"
	}
	return s
}

func (f *flapItem) bump() { f.ver++ }

// EstimatedHeight lets flapItem's height field drive
// list.itemHeightForSum (used by lastOffsetItem / VisibleItemIndices)
// without ever being rendered — mirroring how a real chat item's
// EstimatedHeight tracks its actual content size cheaply.
func (f *flapItem) EstimatedHeight(int) int { return f.height }

var _ list.HeightEstimator = (*flapItem)(nil)

// StartAnimation is called by RestartPausedVisibleAnimations every time
// this item transitions from paused to visible. Production's real
// implementation (chat.baseToolMessageItem / AssistantMessageItem)
// unconditionally returns a's anim.Start(), i.e. arms a brand new
// tea.Tick chain, with no check for whether a previously armed chain for
// this same ID is still in flight. flapItem mirrors that faithfully.
func (f *flapItem) StartAnimation() tea.Cmd {
	f.startCalls++
	return func() tea.Msg { return anim.StepMsg{ID: f.id} }
}

// Animate mirrors a real item's Animate: re-arms its own next tick,
// exactly like anim.Anim.Animate -> Step does. This is what makes a
// single tick chain self-sustaining once started — and, combined with
// StartAnimation's lack of dedup, what makes redundant restarts pile up
// instead of converging back to one chain per item.
func (f *flapItem) Animate(anim.StepMsg) tea.Cmd {
	return func() tea.Msg { return anim.StepMsg{ID: f.id} }
}

var (
	_ chat.MessageItem = (*flapItem)(nil)
	_ chat.Animatable  = (*flapItem)(nil)
)

// TestRestartPausedVisibleAnimations_ThrottlesRestartsOnRepeatedFlap
// guards against a regression of the exact stuck state recovered from a
// real frozen process: the sole event-loop goroutine spinning forever
// through Chat.Animate -> Chat.RestartPausedVisibleAnimations for the
// same two Anim pointers, while ~62k goroutines sat blocked trying to
// deliver stale tea.Tick results back through bubbletea's unbuffered
// message channel (charm.land/bubbletea/v2/tea.go's execBatchMsg /
// handleCommands, confirmed via live goroutine dump with delve).
//
// The mechanism: Chat.ScrollToBottomAndAnimate is called on every
// anim.StepMsg tick (internal/ui/model/ui.go:1099-1103, under Follow)
// AND on every incoming sub-agent event
// (handleChildSessionMessage/updateSubAgentMessage), completely
// independent of whether any previously armed tick chain has actually
// round-tripped back through Update yet — Cmds run on their own
// goroutines and report back asynchronously. Each call recomputes the
// scroll offset via list.ScrollToBottom and then
// RestartPausedVisibleAnimations, which restarts any item that is
// newly visible since the last check. If the item positioned near the
// top of the viewport keeps crossing that boundary — which happens
// naturally while a streamed message at the bottom is growing line by
// line, shifting exactly which older items still fit above it —
// restarting unconditionally on every crossing arms a brand new,
// independent, never-canceled tea.Tick chain each time, on top of any
// earlier chain that hasn't resolved yet: unbounded accumulation of
// live chains for a single animation ID.
//
// The fix throttles restarts per item ID to at most once per
// anim.Interval — the animation's own frame period — so a burst of
// flaps faster than that caps goroutine creation at the animation's
// intended cadence instead of the (potentially much higher and
// unbounded) rate of the underlying event stream.
//
// This test is fully deterministic (call counts and one short,
// interval-bounded sleep — not a flaky wall-clock throughput
// threshold). It exercises both halves of the fix: a rapid burst of
// flaps within a single interval must not spawn more than one restart,
// and the throttle must not permanently strand the item — once the
// interval elapses, the next flap restarts it normally.
func TestRestartPausedVisibleAnimations_ThrottlesRestartsOnRepeatedFlap(t *testing.T) {
	t.Parallel()

	// neighbor sits just above the growing tail item. Its own height is
	// fixed; only its visibility (whether it's inside the viewport)
	// changes, driven by tail's height.
	neighbor := &flapItem{id: "neighbor", height: 1}
	// tail is the actively streaming item: its height alternates as if
	// content were arriving line by line, exactly as a real assistant
	// message's rendered height grows token by token.
	tail := &flapItem{id: "tail", height: 1}

	c := NewChat(newTestUI().com, "")
	c.SetMessages(neighbor, tail)
	// Viewport exactly tall enough for both items at their minimum
	// (1+1=2), but once tail alone grows past the viewport height (3
	// lines), list.lastOffsetItem's backward walk from the bottom stops
	// at tail by itself, skipping neighbor's index entirely — not a
	// partial-scroll clip, a full exit from the visible index range.
	c.SetSize(40, 2)
	c.ScrollToBottom()

	flap := func() {
		// tail "grows": a couple of lines become several, pushing
		// neighbor fully out of the viewport.
		tail.height = 3
		tail.bump()
		// Mirrors ui.go's anim.StepMsg branch: Animate is dispatched
		// for whichever item's Tick just fired, then
		// ScrollToBottomAndAnimate recomputes the offset for the new
		// geometry and restarts anything now visible.
		c.Animate(anim.StepMsg{ID: neighbor.id})
		c.ScrollToBottomAndAnimate()

		// tail "shrinks" back: content wraps differently, or the next
		// chunk is short — neighbor becomes visible again.
		tail.height = 1
		tail.bump()
		c.Animate(anim.StepMsg{ID: neighbor.id})
		c.ScrollToBottomAndAnimate()
	}

	const flaps = 40
	for range flaps {
		flap()
	}

	t.Logf("neighbor item: %d StartAnimation calls across %d rapid flap cycles",
		neighbor.startCalls, flaps)

	// The fix: a burst of flaps that all happen well within a single
	// anim.Interval (~50ms) of wall-clock time must not spawn more
	// than one restart — an uncapped restart-per-flap is exactly the
	// multiplying mechanism that produced ~62k goroutines stuck in
	// bubbletea's execBatchMsg/handleCommands in the live frozen
	// process.
	require.LessOrEqualf(t, neighbor.startCalls, 1,
		"expected at most one StartAnimation call for a burst of flaps "+
			"within a single animation interval (got %d across %d "+
			"flaps); an uncapped restart-per-flap reproduces the "+
			"goroutine-pileup mechanism found in the live frozen process",
		neighbor.startCalls, flaps)

	// The throttle must not permanently strand the item: once the
	// animation interval has elapsed, the next visibility flap should
	// be free to restart it again.
	time.Sleep(anim.Interval() + 5*time.Millisecond)
	before := neighbor.startCalls
	flap()
	require.Equal(t, before+1, neighbor.startCalls,
		"after the throttle interval elapses, a new visibility flap "+
			"should restart the animation again")
}
