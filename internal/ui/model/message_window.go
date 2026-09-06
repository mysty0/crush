package model

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// initialMessageWindow is how many recent messages a session loads on
// open. Extended automatically to include everything since the
// session's last compaction (see workspace.Workspace.ListMessagesWindow)
// so the initial view always shows the full active context an agent
// turn would still send to the model, even when that's longer than
// this default.
//
// This exists because rewind forks copy a session's entire prior
// history rather than trimming it, so a session compacted many times
// over can accumulate tens of thousands of messages -- loading all of
// them into memory on every open is what made opening one such session
// use about a gigabyte of RAM for content that was never going to be
// scrolled back to.
const initialMessageWindow = 300

// olderMessagesPageSize is how many messages are fetched per lazy
// "scrolled to top" page-in (see UI.maybeLoadOlderMessages).
const olderMessagesPageSize = 150

// defaultChatHistoryRAMWindow is the fallback resident cap (in chat
// items, i.e. rendered pieces of a message -- not raw DB messages) for
// lazily-loaded older pages when Options.TUI.ChatHistoryRAMWindow is
// unset. See maybeEvictHistory.
const defaultChatHistoryRAMWindow = 1000

// loadedPage records one lazily-loaded older page still resident in
// the chat: how many chat items it contributed, and the CreatedAt of
// its oldest message (the paging cursor to fall back to if the page is
// evicted). Pages are evicted as an all-or-nothing unit rather than by
// individual message, so the paging cursor recorded here is always
// exact -- there's never a partial page to reconstruct a boundary for.
type loadedPage struct {
	itemCount       int
	oldestCreatedAt int64
}

// messageWindowState tracks the main session chat's windowed
// message-loading state. Sub-agent/workflow chat views are not
// windowed -- they're short-lived and never approach the sizes that
// make this worth the complexity.
type messageWindowState struct {
	// sessionID is which session this state belongs to, so a lazy-load
	// result that lands after the user has switched sessions is
	// recognized as stale and dropped instead of corrupting the chat
	// now showing a different session.
	sessionID string
	// oldestLoaded is the CreatedAt of the oldest message currently
	// loaded into m.chat, i.e. the cursor for the next older-page fetch.
	oldestLoaded int64
	// initialOldest is oldestLoaded's value right after the session's
	// initial windowed load, before any lazy paging happened. It is the
	// fallback oldestLoaded reverts to once every lazily-loaded page has
	// been evicted (see maybeEvictHistory) -- the initial window itself
	// (which always includes the full active post-compaction context)
	// is never an eviction candidate.
	initialOldest int64
	// hasMore reports whether the last window/page fetch found more
	// history beyond what was loaded. Starts false so a session that
	// hasn't finished its initial load yet never triggers a fetch.
	hasMore bool
	// loading is true while an older-page fetch is in flight, so a
	// user holding PageUp (or a fast wheel scroll) doesn't fan out
	// multiple redundant fetches for the same page.
	loading bool
	// pages records each lazily-loaded older page still resident in the
	// chat, most-recently-loaded first -- which, since every page is
	// prepended, is also the page currently nearest the front of the
	// chat list. Only these pages are eviction candidates; see
	// maybeEvictHistory.
	pages []loadedPage
}

// olderMessagesLoadedMsg carries the result of an off-thread
// UI.maybeLoadOlderMessages fetch.
type olderMessagesLoadedMsg struct {
	sessionID string
	messages  []message.Message
	hasMore   bool
	// err is set when the fetch itself failed. hasMore stays true in
	// that case (see maybeLoadOlderMessages) so a later scroll-to-top
	// simply retries rather than giving up on the session's history
	// after one transient failure.
	err error
}

// maybeLoadOlderMessages checks whether the main session chat is
// scrolled to the top and there is more history to page in, and if so
// dispatches an off-thread fetch for the next older page. A safe no-op
// otherwise (wrong view, a fetch already in flight, nothing more to
// load, or not actually at the top), so it can be called after every
// scroll-affecting key or wheel event without needing to know which
// ones might have reached the top.
func (m *UI) maybeLoadOlderMessages() tea.Cmd {
	if m.state != uiChat || !m.hasSession() || m.subAgentSessionID != "" {
		return nil
	}
	if m.msgWindow.sessionID != m.session.ID || !m.msgWindow.hasMore || m.msgWindow.loading {
		return nil
	}
	if !m.chat.AtTop() {
		return nil
	}

	m.msgWindow.loading = true
	sessionID := m.session.ID
	before := m.msgWindow.oldestLoaded
	return func() tea.Msg {
		older, err := m.com.Workspace.ListOlderMessages(context.Background(), sessionID, before, olderMessagesPageSize)
		if err != nil {
			// Always route through olderMessagesLoadedMsg, even on
			// error: it's the only place that clears msgWindow.loading,
			// and a bare util.ReportError here would leave it stuck
			// true forever, permanently disabling further paging for
			// this session.
			return olderMessagesLoadedMsg{sessionID: sessionID, hasMore: true, err: err}
		}
		return olderMessagesLoadedMsg{
			sessionID: sessionID,
			messages:  older,
			hasMore:   len(older) > 0,
		}
	}
}

// applyOlderMessages prepends a lazily-loaded older page to the chat.
// Dropped if the session has since changed out from under the fetch.
func (m *UI) applyOlderMessages(msg olderMessagesLoadedMsg) tea.Cmd {
	m.msgWindow.loading = false
	if !m.hasSession() || msg.sessionID != m.session.ID {
		return nil
	}
	m.msgWindow.hasMore = msg.hasMore
	if msg.err != nil {
		return util.ReportError(msg.err)
	}
	if len(msg.messages) == 0 {
		return nil
	}
	m.msgWindow.oldestLoaded = msg.messages[0].CreatedAt

	// Discard the second return value: an older page is, by
	// definition, further in the past than anything already loaded, so
	// it must never move m.lastUserMessageTime forward.
	items, _ := m.buildMessageItems(msg.messages, 0)
	m.loadNestedToolCalls(items)
	m.chat.PrependMessages(items...)
	m.msgWindow.pages = append([]loadedPage{{
		itemCount:       len(items),
		oldestCreatedAt: m.msgWindow.oldestLoaded,
	}}, m.msgWindow.pages...)
	m.updateLayoutAndSize()
	return nil
}

// chatHistoryRAMWindow resolves the configured resident cap for
// maybeEvictHistory, falling back to defaultChatHistoryRAMWindow when
// unset.
func (m *UI) chatHistoryRAMWindow() int {
	cfg := m.com.Config()
	if cfg == nil || cfg.Options == nil || cfg.Options.TUI == nil || cfg.Options.TUI.ChatHistoryRAMWindow == nil {
		return defaultChatHistoryRAMWindow
	}
	if v := *cfg.Options.TUI.ChatHistoryRAMWindow; v != 0 {
		return v
	}
	return defaultChatHistoryRAMWindow
}

// maybeEvictHistory bounds how much lazily-loaded older history stays
// resident in memory once it has scrolled out of view. A no-op unless
// the resident item count exceeds the configured cap (see
// chatHistoryRAMWindow) and the oldest resident page is entirely
// off-screen. Evicted pages are simply re-fetched from disk if the
// user scrolls back up to them (see maybeLoadOlderMessages), so this
// never loses anything -- it only frees memory for content nobody is
// currently looking at. The initial window loaded when the session was
// opened, including the full active post-compaction context, is never
// a candidate: only pages paged in afterward by scrolling up are.
func (m *UI) maybeEvictHistory() {
	if m.state != uiChat || !m.hasSession() || m.subAgentSessionID != "" {
		return
	}
	if m.msgWindow.sessionID != m.session.ID || len(m.msgWindow.pages) == 0 {
		return
	}
	capCount := m.chatHistoryRAMWindow()
	if capCount < 0 {
		return
	}

	for len(m.msgWindow.pages) > 0 {
		resident := 0
		for _, p := range m.msgWindow.pages {
			resident += p.itemCount
		}
		if resident <= capCount {
			return
		}

		// pages[0] is the most-recently-loaded page, which is also the
		// one currently nearest the front of the chat list (index 0) --
		// see the pages field doc. That's exactly what EvictFront
		// removes, and it's the safest pick: it's the page the user
		// scrolled to least long ago, so scrolling back down even a
		// little already makes it invisible.
		frontPage := m.msgWindow.pages[0]
		if !m.chat.EvictFront(frontPage.itemCount) {
			// It's (at least partly) visible; nothing more can be
			// safely evicted right now.
			return
		}
		m.msgWindow.pages = m.msgWindow.pages[1:]
		m.msgWindow.hasMore = true
		if len(m.msgWindow.pages) > 0 {
			m.msgWindow.oldestLoaded = m.msgWindow.pages[0].oldestCreatedAt
		} else {
			m.msgWindow.oldestLoaded = m.msgWindow.initialOldest
		}
	}
}
