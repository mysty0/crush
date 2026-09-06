package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
)

// olderMessagesWorkspace extends countingWorkspace with a controllable
// ListOlderMessages so message_window tests can simulate a real page of
// history landing (or a fetch failing) without touching a database.
type olderMessagesWorkspace struct {
	*countingWorkspace

	olderCalls  int
	olderResult []message.Message
	olderErr    error
}

func (w *olderMessagesWorkspace) ListOlderMessages(_ context.Context, _ string, _ int64, _ int) ([]message.Message, error) {
	w.olderCalls++
	return w.olderResult, w.olderErr
}

// seedManyMessages fills the chat with enough single-line items to make
// the list scrollable at the fixture's 45-line height, then scrolls to
// the top so AtTop() is meaningfully false-by-default (bottom-anchored)
// until a test explicitly scrolls up.
func seedManyMessages(t *testing.T, m *UI, n int) {
	t.Helper()
	msgs := make([]message.Message, n)
	for i := range n {
		msgs[i] = message.Message{
			ID:        "seed-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Role:      message.User,
			Parts:     []message.ContentPart{message.TextContent{Text: "message"}},
			CreatedAt: int64(1000 + i),
		}
	}
	items, _ := m.buildMessageItems(msgs, 0)
	m.chat.SetMessages(items...)
}

// TestMaybeLoadOlderMessagesSkipsWhenNoMore pins the cheapest guard: a
// session that has already loaded everything (hasMore=false) must never
// probe the workspace, however the chat is scrolled.
func TestMaybeLoadOlderMessagesSkipsWhenNoMore(t *testing.T) {
	base := &countingWorkspace{ready: true}
	ws := &olderMessagesWorkspace{countingWorkspace: base}
	m := newBusyUI(base)
	m.com.Workspace = ws
	m.msgWindow = messageWindowState{sessionID: m.session.ID, hasMore: false}

	require.Nil(t, m.maybeLoadOlderMessages())
	require.Zero(t, ws.olderCalls)
}

// TestMaybeLoadOlderMessagesSkipsWhenAlreadyLoading guards against
// fanning out duplicate fetches for the same page when the user holds a
// scroll key or scrolls fast with the wheel.
func TestMaybeLoadOlderMessagesSkipsWhenAlreadyLoading(t *testing.T) {
	base := &countingWorkspace{ready: true}
	ws := &olderMessagesWorkspace{countingWorkspace: base}
	m := newBusyUI(base)
	m.com.Workspace = ws
	m.msgWindow = messageWindowState{sessionID: m.session.ID, hasMore: true, loading: true}

	require.Nil(t, m.maybeLoadOlderMessages())
	require.Zero(t, ws.olderCalls)
}

// TestMaybeLoadOlderMessagesSkipsWhenNotAtTop pins the scroll gate: a
// session with plenty of loaded history that the user hasn't scrolled up
// through yet (still anchored at the bottom, following new output) must
// not eagerly fetch older pages nobody asked to see.
func TestMaybeLoadOlderMessagesSkipsWhenNotAtTop(t *testing.T) {
	base := &countingWorkspace{ready: true}
	ws := &olderMessagesWorkspace{countingWorkspace: base}
	m := newBusyUI(base)
	m.com.Workspace = ws
	m.msgWindow = messageWindowState{sessionID: m.session.ID, hasMore: true}
	seedManyMessages(t, m, 200) // SetMessages leaves the list bottom-anchored

	require.False(t, m.chat.AtTop())
	require.Nil(t, m.maybeLoadOlderMessages())
	require.Zero(t, ws.olderCalls)
}

// TestMaybeLoadOlderMessagesFetchesWhenAtTop is the positive case: once
// the user has scrolled all the way up through a session with more
// history available, the next scroll-triggered check must dispatch a
// fetch, and only one -- the loading flag must already be set
// synchronously so a second call before the result lands is a no-op.
func TestMaybeLoadOlderMessagesFetchesWhenAtTop(t *testing.T) {
	base := &countingWorkspace{ready: true}
	older := []message.Message{
		{ID: "o1", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "older"}}, CreatedAt: 500},
	}
	ws := &olderMessagesWorkspace{countingWorkspace: base, olderResult: older}
	m := newBusyUI(base)
	m.com.Workspace = ws
	m.msgWindow = messageWindowState{sessionID: m.session.ID, hasMore: true, oldestLoaded: 1000}
	seedManyMessages(t, m, 200)
	m.chat.ScrollToTop()
	require.True(t, m.chat.AtTop())

	cmd := m.maybeLoadOlderMessages()
	require.NotNil(t, cmd)
	require.True(t, m.msgWindow.loading, "loading must be set synchronously, before the fetch runs")

	require.Nil(t, m.maybeLoadOlderMessages(), "a second check while loading must not fetch again")
	require.Zero(t, ws.olderCalls, "the first fetch hasn't run yet -- only invoking cmd() does")

	result := cmd()
	loaded, ok := result.(olderMessagesLoadedMsg)
	require.True(t, ok)
	require.Equal(t, m.session.ID, loaded.sessionID)
	require.Equal(t, older, loaded.messages)
	require.True(t, loaded.hasMore)
	require.Equal(t, 1, ws.olderCalls)
}

// TestApplyOlderMessagesPrependsAndAdvancesCursor verifies the happy
// path end to end: a loaded page grows the chat, moves the paging
// cursor back to the new oldest message, and clears the loading flag so
// the next scroll-to-top can fetch again.
func TestApplyOlderMessagesPrependsAndAdvancesCursor(t *testing.T) {
	base := &countingWorkspace{ready: true}
	m := newBusyUI(base)
	m.com.Workspace = &olderMessagesWorkspace{countingWorkspace: base}
	seedManyMessages(t, m, 5)
	before := m.chat.Len()

	m.msgWindow = messageWindowState{sessionID: m.session.ID, loading: true, oldestLoaded: 1000}
	older := []message.Message{
		{ID: "o1", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "older one"}}, CreatedAt: 100},
		{ID: "o2", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "older two"}}, CreatedAt: 200},
	}

	cmd := m.applyOlderMessages(olderMessagesLoadedMsg{
		sessionID: m.session.ID,
		messages:  older,
		hasMore:   true,
	})
	require.Nil(t, cmd)
	require.False(t, m.msgWindow.loading)
	require.True(t, m.msgWindow.hasMore)
	require.Equal(t, int64(100), m.msgWindow.oldestLoaded,
		"the cursor must move to the new oldest loaded message")
	require.Equal(t, before+2, m.chat.Len(), "the page's items must be prepended, not replace anything")
}

// TestApplyOlderMessagesDropsStaleSessionResult pins a race guard: a
// fetch started for one session must not corrupt the chat if the user
// has since switched to a different session before it lands. The
// loading flag still clears, since it belongs to the session that was
// loading, and that session isn't the one asking anymore either way.
func TestApplyOlderMessagesDropsStaleSessionResult(t *testing.T) {
	base := &countingWorkspace{ready: true}
	m := newBusyUI(base)
	m.com.Workspace = &olderMessagesWorkspace{countingWorkspace: base}
	seedManyMessages(t, m, 5)
	before := m.chat.Len()
	m.msgWindow = messageWindowState{sessionID: "other-session", loading: true}

	cmd := m.applyOlderMessages(olderMessagesLoadedMsg{
		sessionID: "stale-session",
		messages: []message.Message{
			{ID: "o1", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "x"}}, CreatedAt: 1},
		},
	})
	require.Nil(t, cmd)
	require.False(t, m.msgWindow.loading)
	require.Equal(t, before, m.chat.Len(), "a stale result must never touch the chat")
}

// TestApplyOlderMessagesErrorClearsLoadingAndKeepsRetryable pins the bug
// this would otherwise reintroduce: a failed fetch must still clear the
// loading flag (or paging is permanently stuck for the rest of the
// session) and must keep hasMore true so the next scroll-to-top retries
// instead of silently giving up on the session's history forever.
func TestApplyOlderMessagesErrorClearsLoadingAndKeepsRetryable(t *testing.T) {
	base := &countingWorkspace{ready: true}
	m := newBusyUI(base)
	m.com.Workspace = &olderMessagesWorkspace{countingWorkspace: base}
	m.msgWindow = messageWindowState{sessionID: m.session.ID, loading: true, hasMore: true}

	cmd := m.applyOlderMessages(olderMessagesLoadedMsg{
		sessionID: m.session.ID,
		hasMore:   true,
		err:       context.DeadlineExceeded,
	})
	require.NotNil(t, cmd, "an error result must still report something to the user")
	require.False(t, m.msgWindow.loading)
	require.True(t, m.msgWindow.hasMore, "a transient failure must not permanently disable paging")

	// The returned command must be the error report, not a panic or a
	// stray olderMessagesLoadedMsg loop.
	msg := cmd()
	_, isTeaMsg := msg.(tea.Msg)
	require.True(t, isTeaMsg)
}

// evictConfigWorkspace overrides Config() to control
// Options.TUI.ChatHistoryRAMWindow for maybeEvictHistory tests.
type evictConfigWorkspace struct {
	*countingWorkspace
	cfg *config.Config
}

func (w *evictConfigWorkspace) Config() *config.Config { return w.cfg }

func evictTestConfig(ramWindow int) *config.Config {
	return &config.Config{Options: &config.Options{TUI: &config.TUIOptions{ChatHistoryRAMWindow: &ramWindow}}}
}

// TestMaybeEvictHistorySkipsUnderCap pins the cheap no-op path: while
// resident lazily-loaded pages stay under the configured cap, nothing
// is evicted -- there's no point discarding content that fits in the
// budget just because it scrolled out of view.
func TestMaybeEvictHistorySkipsUnderCap(t *testing.T) {
	base := &countingWorkspace{ready: true}
	m := newBusyUI(base)
	m.com.Workspace = &evictConfigWorkspace{countingWorkspace: base, cfg: evictTestConfig(1000)}
	seedManyMessages(t, m, 50)
	m.msgWindow = messageWindowState{
		sessionID: m.session.ID,
		pages:     []loadedPage{{itemCount: 10, oldestCreatedAt: 1}},
	}
	before := m.chat.Len()

	m.maybeEvictHistory()

	require.Equal(t, before, m.chat.Len())
	require.Len(t, m.msgWindow.pages, 1)
}

// TestMaybeEvictHistoryRemovesInvisibleFrontPage is the positive case:
// once resident lazily-loaded pages exceed the cap and the oldest one
// has scrolled out of view (the chat is bottom-anchored, following new
// output), it's evicted, the chat shrinks by exactly that page's item
// count, and the paging cursor falls back to the next page (or the
// initial window's own boundary once every page is gone) with hasMore
// forced true so scrolling back up re-fetches it.
func TestMaybeEvictHistoryRemovesInvisibleFrontPage(t *testing.T) {
	base := &countingWorkspace{ready: true}
	m := newBusyUI(base)
	m.com.Workspace = &evictConfigWorkspace{countingWorkspace: base, cfg: evictTestConfig(15)}
	seedManyMessages(t, m, 50) // bottom-anchored: the front is off-screen
	before := m.chat.Len()
	m.msgWindow = messageWindowState{
		sessionID:     m.session.ID,
		hasMore:       false,
		oldestLoaded:  1,
		initialOldest: 1000,
		pages: []loadedPage{
			{itemCount: 10, oldestCreatedAt: 1}, // nearest the front (index 0) -- evicted first
			{itemCount: 10, oldestCreatedAt: 500},
		},
	}

	m.maybeEvictHistory()

	require.Equal(t, before-10, m.chat.Len(), "exactly the front page's items must be removed")
	require.Len(t, m.msgWindow.pages, 1)
	require.True(t, m.msgWindow.hasMore, "evicting must re-flag more history as available")
	require.Equal(t, int64(500), m.msgWindow.oldestLoaded, "the cursor must fall back to the next resident page")
}

// TestMaybeEvictHistoryStopsAtVisiblePage guards the core safety
// property: eviction never removes anything currently on screen, even
// if that leaves the resident count over the configured cap. Here the
// only resident page is small enough to be entirely visible (the chat
// isn't scrolled anywhere), so EvictFront must refuse and
// maybeEvictHistory must leave everything alone.
func TestMaybeEvictHistoryStopsAtVisiblePage(t *testing.T) {
	base := &countingWorkspace{ready: true}
	m := newBusyUI(base)
	m.com.Workspace = &evictConfigWorkspace{countingWorkspace: base, cfg: evictTestConfig(1)}
	seedManyMessages(t, m, 3) // fits entirely on screen at height 45
	before := m.chat.Len()
	m.msgWindow = messageWindowState{
		sessionID: m.session.ID,
		pages:     []loadedPage{{itemCount: before, oldestCreatedAt: 1}},
	}

	m.maybeEvictHistory()

	require.Equal(t, before, m.chat.Len(), "a visible page must never be evicted")
	require.Len(t, m.msgWindow.pages, 1)
}

// TestMaybeEvictHistoryIgnoresNegativeWindow pins the escape hatch: a
// negative Options.TUI.ChatHistoryRAMWindow disables eviction entirely,
// however far over any implicit cap the resident set grows.
func TestMaybeEvictHistoryIgnoresNegativeWindow(t *testing.T) {
	base := &countingWorkspace{ready: true}
	m := newBusyUI(base)
	m.com.Workspace = &evictConfigWorkspace{countingWorkspace: base, cfg: evictTestConfig(-1)}
	seedManyMessages(t, m, 50)
	before := m.chat.Len()
	m.msgWindow = messageWindowState{
		sessionID: m.session.ID,
		pages:     []loadedPage{{itemCount: 10, oldestCreatedAt: 1}},
	}

	m.maybeEvictHistory()

	require.Equal(t, before, m.chat.Len())
	require.Len(t, m.msgWindow.pages, 1)
}
