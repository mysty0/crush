package model

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/stretchr/testify/require"
)

// visualTestItem is a minimal chat item implementing list.Highlightable so
// keyboard visual-mode selection tests can exercise the real
// getHighlightRange/HighlightContent pipeline with fully predictable
// content, independent of glamour's markdown rendering (leading margins,
// padding, wrapping).
type visualTestItem struct {
	id    string
	lines []string

	startLine, startCol, endLine, endCol int
}

func newVisualTestItem(id string, lines ...string) *visualTestItem {
	return &visualTestItem{id: id, lines: lines, startLine: -1, startCol: -1, endLine: -1, endCol: -1}
}

func (v *visualTestItem) ID() string      { return v.id }
func (v *visualTestItem) Version() uint64 { return 0 }
func (v *visualTestItem) Finished() bool  { return true }
func (v *visualTestItem) SetFocused(bool) {}

func (v *visualTestItem) RawRender(int) string {
	return strings.Join(v.lines, "\n")
}

// Render mirrors the left-gutter prefix every real message item adds on
// top of its RawRender content, since getHighlightRange's viewport-space
// math always assumes that offset.
func (v *visualTestItem) Render(int) string {
	prefix := strings.Repeat(" ", chat.MessageLeftPaddingTotal)
	lines := make([]string, len(v.lines))
	for i, l := range v.lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// SetHighlight mirrors highlightableMessageItem's contract: the caller
// passes viewport-space columns (including the left gutter offset), and
// the item converts them to content-space before storing.
func (v *visualTestItem) SetHighlight(startLine, startCol, endLine, endCol int) {
	offset := chat.MessageLeftPaddingTotal
	v.startLine = startLine
	v.startCol = max(0, startCol-offset)
	v.endLine = endLine
	if endCol >= 0 {
		v.endCol = max(0, endCol-offset)
	} else {
		v.endCol = endCol
	}
}

func (v *visualTestItem) Highlight() (int, int, int, int) {
	return v.startLine, v.startCol, v.endLine, v.endCol
}

var (
	_ chat.MessageItem   = (*visualTestItem)(nil)
	_ list.Highlightable = (*visualTestItem)(nil)
	_ list.RawRenderable = (*visualTestItem)(nil)
)

func keyMsg(s string) tea.KeyPressMsg {
	if len(s) == 1 {
		return tea.KeyPressMsg{Text: s, Code: rune(s[0])}
	}
	switch s {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}
	return tea.KeyPressMsg{Text: s}
}

func focusedTestChat(t *testing.T, item *visualTestItem) *Chat {
	t.Helper()
	u := newTestUI()
	u.chat.SetMessages(item)
	u.updateLayoutAndSize()
	u.chat.Focus()
	u.chat.SetSelected(0)
	return u.chat
}

// renderChat forces a draw pass so the registered applyHighlightRange
// callback pushes the current highlight range onto the list items —
// HighlightContent (like the real UI) only reflects state that has been
// painted at least once.
func renderChat(t *testing.T, c *Chat) {
	t.Helper()
	_ = renderToBuffer(t, c, 80, 20)
}

func TestChatVisualMode_EnterExit(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello world"))

	require.False(t, c.InVisualMode())
	require.True(t, c.EnterVisual(false))
	require.True(t, c.InVisualMode())

	handled, cmd := c.HandleVisualKeyMsg(keyMsg("esc"))
	require.True(t, handled)
	require.Nil(t, cmd)
	require.False(t, c.InVisualMode(), "esc must exit visual mode without yanking")
}

func TestChatVisualMode_CursorOnlyBeforeLock(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello world"))
	require.True(t, c.EnterVisual(false))

	// Before pressing "v" a second time, movement only repositions the
	// cursor (shown as a single-character block); it must not grow a
	// selection.
	_, _ = c.HandleVisualKeyMsg(keyMsg("w"))
	renderChat(t, c)
	require.Equal(t, "w", c.HighlightContent(), "movement before locking must not select more than the cursor cell")

	// Locking with "v" and then moving grows a real selection from here.
	handled, cmd := c.HandleVisualKeyMsg(keyMsg("v"))
	require.True(t, handled)
	require.Nil(t, cmd)
	_, _ = c.HandleVisualKeyMsg(keyMsg("e"))
	renderChat(t, c)
	require.Equal(t, "world", c.HighlightContent())
}

func TestChatVisualMode_WordMotionAndYank(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello world"))
	require.True(t, c.EnterVisual(false))

	// Lock the anchor at the starting position (0,0) before extending.
	_, _ = c.HandleVisualKeyMsg(keyMsg("v"))

	// Extend the selection to the end of "hello" with "e".
	handled, cmd := c.HandleVisualKeyMsg(keyMsg("e"))
	require.True(t, handled)
	require.Nil(t, cmd)
	renderChat(t, c)
	require.Equal(t, "hello", c.HighlightContent())

	// Extend further with "w" (jump to start of "world") then "$" (end
	// of line) to select the whole line.
	_, _ = c.HandleVisualKeyMsg(keyMsg("w"))
	_, _ = c.HandleVisualKeyMsg(keyMsg("$"))
	renderChat(t, c)
	require.Equal(t, "hello world", c.HighlightContent())

	// Yank copies and exits visual mode.
	handled, cmd = c.HandleVisualKeyMsg(keyMsg("y"))
	require.True(t, handled)
	require.NotNil(t, cmd, "yank must return a clipboard copy command")
}

func TestChatVisualMode_BackwardSelection(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello world"))
	require.True(t, c.EnterVisual(false))

	// Move the cursor to the start of "world" (still cursor-only, so
	// nothing is selected beyond the cursor cell), then lock the anchor
	// there with "v".
	_, _ = c.HandleVisualKeyMsg(keyMsg("w"))
	_, _ = c.HandleVisualKeyMsg(keyMsg("v"))

	// Move back with "b" to the start of "hello": the selection now
	// spans from the cursor back to the locked anchor, inclusive of
	// both ends ("hello w").
	_, _ = c.HandleVisualKeyMsg(keyMsg("b"))
	renderChat(t, c)
	require.Equal(t, "hello w", c.HighlightContent())
}

func TestChatVisualMode_LineWise(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello", "world"))
	require.True(t, c.EnterVisual(true))

	_, _ = c.HandleVisualKeyMsg(keyMsg("j"))
	renderChat(t, c)
	text := c.HighlightContent()
	require.Contains(t, text, "hello")
	require.Contains(t, text, "world")
}

func TestChatVisualMode_MultiLineWordMotion(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello", "world"))
	require.True(t, c.EnterVisual(false))
	_, _ = c.HandleVisualKeyMsg(keyMsg("v")) // lock the anchor at (0,0)

	// "w" from the last word of line 0 should wrap to line 1's first
	// word, landing the cursor on its first character.
	_, _ = c.HandleVisualKeyMsg(keyMsg("w"))
	renderChat(t, c)
	require.Equal(t, "hello\nw", c.HighlightContent())
}

func TestChatVisualMode_UnrecognizedKeyIsSwallowed(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello world"))
	require.True(t, c.EnterVisual(false))

	handled, cmd := c.HandleVisualKeyMsg(keyMsg("z"))
	require.True(t, handled, "unrecognized keys must be swallowed while visual mode is active")
	require.Nil(t, cmd)
	require.True(t, c.InVisualMode())
}

func TestChatVisualMode_StartsFromClickPosition(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello world"))

	// Simulate a single click landing on "world" (content col 6), then
	// entering visual mode should start there instead of at (0,0).
	clickCol := 6 + chat.MessageLeftPaddingTotal
	handled, _ := c.HandleMouseDown(clickCol, 0)
	require.True(t, handled)

	require.True(t, c.EnterVisual(false))
	_, _ = c.HandleVisualKeyMsg(keyMsg("v")) // lock the anchor at the click position
	_, _ = c.HandleVisualKeyMsg(keyMsg("e"))
	renderChat(t, c)
	require.Equal(t, "world", c.HighlightContent())
}

func TestChatVisualMode_ReenterResumesLastPosition(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello world"))
	require.True(t, c.EnterVisual(false))

	// Move to the start of "world", then leave visual mode.
	_, _ = c.HandleVisualKeyMsg(keyMsg("w"))
	handled, _ := c.HandleVisualKeyMsg(keyMsg("esc"))
	require.True(t, handled)
	require.False(t, c.InVisualMode())

	// Re-entering visual mode on the same item resumes from that
	// position rather than resetting to the top.
	require.True(t, c.EnterVisual(false))
	_, _ = c.HandleVisualKeyMsg(keyMsg("v")) // lock the anchor here
	_, _ = c.HandleVisualKeyMsg(keyMsg("e"))
	renderChat(t, c)
	require.Equal(t, "world", c.HighlightContent())
}

func TestChatVisualMode_ToggleVLeavesLineWise(t *testing.T) {
	t.Parallel()

	c := focusedTestChat(t, newVisualTestItem("m1", "hello world"))
	require.True(t, c.EnterVisual(true))
	require.True(t, c.kbVisualLine)

	handled, _ := c.HandleVisualKeyMsg(keyMsg("v"))
	require.True(t, handled)
	require.True(t, c.InVisualMode(), "toggling from V to v stays in visual mode")
	require.False(t, c.kbVisualLine)

	handled, _ = c.HandleVisualKeyMsg(keyMsg("v"))
	require.True(t, handled)
	require.False(t, c.InVisualMode(), "v while already char-wise exits visual mode")
}
