package model

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/x/ansi"
)

// InVisualMode reports whether keyboard visual-selection mode ("v"/"V") is
// currently active.
func (m *Chat) InVisualMode() bool {
	return m.kbVisualActive
}

// EnterVisual starts keyboard visual-selection mode on the currently
// selected item: character-wise ("v") when lineWise is false, line-wise
// ("V") when true. Character-wise entry starts in the cursor-only
// sub-state (see the [Chat] field docs); line-wise entry starts selecting
// immediately. The starting position is the last remembered position for
// this item (see cursorItem on [Chat]) — typically wherever the user
// last clicked or left off — so navigation can start mid-message instead
// of always resetting to the top. Returns false and does nothing if
// there is no focused, highlightable, non-empty item to select within.
func (m *Chat) EnterVisual(lineWise bool) bool {
	if !m.list.Focused() {
		return false
	}
	idx := m.list.Selected()
	item := m.list.ItemAt(idx)
	if item == nil {
		return false
	}
	if _, ok := item.(list.Highlightable); !ok {
		return false
	}
	lines := m.visualLines(idx)
	if len(lines) == 0 {
		return false
	}

	line, col := 0, 0
	if m.cursorItem == idx {
		line = clampInt(m.cursorLine, 0, len(lines)-1)
		col = clampInt(m.cursorCol, 0, lineLastCol(lines[line]))
	}

	m.kbVisualActive = true
	m.kbVisualLine = lineWise
	m.kbSelecting = lineWise
	m.kbVisualItem = idx
	m.kbAnchorLine, m.kbAnchorCol = line, col
	m.kbCursorLine, m.kbCursorCol = line, col
	return true
}

// ExitVisual leaves keyboard visual-selection mode, clearing any
// highlight it was showing on the next render.
func (m *Chat) ExitVisual() {
	m.kbVisualActive = false
	m.kbVisualLine = false
	m.kbSelecting = false
}

// visualLines returns the ansi-stripped rendered lines of the item at
// idx — the same content-space coordinates used by mouse-driven
// selection (selectWord/selectLine).
func (m *Chat) visualLines(idx int) []string {
	item := m.list.ItemAt(idx)
	if item == nil {
		return nil
	}
	var rendered string
	if rr, ok := item.(list.RawRenderable); ok {
		rendered = rr.RawRender(m.list.Width())
	} else {
		rendered = item.Render(m.list.Width())
	}
	lines := strings.Split(rendered, "\n")
	for i, l := range lines {
		lines[i] = ansi.Strip(l)
	}
	return lines
}

// keyboardHighlightRange computes the current highlight range from
// keyboard visual-mode state, in the same (item, line, viewport-col)
// coordinate space getHighlightRange's mouse path returns (consumed by
// applyHighlightRange/HighlightContent). The keyboard cursor and anchor
// are vim-style inclusive character positions; the later endpoint is
// extended by one display column so the character under the cursor block
// is always included, matching vim visual mode. Line-wise mode ("V")
// selects whole lines using the -1 "end of line" sentinel that
// HighlightBuffer already supports.
func (m *Chat) keyboardHighlightRange() (startItemIdx, startLine, startCol, endItemIdx, endLine, endCol int) {
	offset := chat.MessageLeftPaddingTotal

	forward := m.kbCursorLine > m.kbAnchorLine ||
		(m.kbCursorLine == m.kbAnchorLine && m.kbCursorCol >= m.kbAnchorCol)

	sLine, sCol := m.kbAnchorLine, m.kbAnchorCol
	eLine, eCol := m.kbCursorLine, m.kbCursorCol
	if !forward {
		sLine, sCol, eLine, eCol = eLine, eCol, sLine, sCol
	}

	if m.kbVisualLine {
		return m.kbVisualItem, sLine, offset, m.kbVisualItem, eLine, -1
	}
	return m.kbVisualItem, sLine, sCol + offset, m.kbVisualItem, eLine, eCol + 1 + offset
}

// HandleVisualKeyMsg processes a key press while keyboard visual-selection
// mode is active. It always returns handled=true so navigation/scrolling
// bindings underneath do not leak through mid-selection; unrecognized
// keys are simply swallowed (a no-op), matching vim's own behavior.
func (m *Chat) HandleVisualKeyMsg(key tea.KeyMsg) (bool, tea.Cmd) {
	if !m.kbVisualActive {
		return false, nil
	}
	lines := m.visualLines(m.kbVisualItem)
	if len(lines) == 0 {
		m.ExitVisual()
		return true, nil
	}
	line := clampInt(m.kbCursorLine, 0, len(lines)-1)
	col := m.kbCursorCol

	switch key.String() {
	case "esc":
		m.ExitVisual()
		return true, nil
	case "v":
		switch {
		case m.kbVisualLine:
			// Downgrade from line-wise to character-wise while keeping
			// the current selection active.
			m.kbVisualLine = false
		case !m.kbSelecting:
			// First "v" only positioned the cursor. Lock the anchor
			// here and start growing a real selection on the next move.
			m.kbSelecting = true
		default:
			// Already selecting character-wise: exit.
			m.ExitVisual()
		}
		return true, nil
	case "V":
		// Pressing V always (re)starts a line-wise selection anchored at
		// the current cursor position, whether or not one was already
		// active.
		m.kbVisualLine = true
		m.kbSelecting = true
		return true, nil
	case "y":
		return true, m.YankVisual()
	case "h", "left":
		if col > 0 {
			col--
		}
	case "l", "right":
		if max := lineLastCol(lines[line]); col < max {
			col++
		}
	case "j", "down":
		if line < len(lines)-1 {
			line++
			col = clampInt(col, 0, lineLastCol(lines[line]))
		}
	case "k", "up":
		if line > 0 {
			line--
			col = clampInt(col, 0, lineLastCol(lines[line]))
		}
	case "w":
		line, col = wordForward(lines, line, col, false)
	case "W":
		line, col = wordForward(lines, line, col, true)
	case "b":
		line, col = wordBackward(lines, line, col, false)
	case "B":
		line, col = wordBackward(lines, line, col, true)
	case "e":
		line, col = wordEnd(lines, line, col, false)
	case "E":
		line, col = wordEnd(lines, line, col, true)
	case "0":
		col = 0
	case "^":
		col = lineFirstNonBlankCol(lines[line])
	case "$":
		col = lineLastCol(lines[line])
	case "g":
		line, col = 0, 0
	case "G":
		line = len(lines) - 1
		col = lineFirstNonBlankCol(lines[line])
	default:
		return true, nil
	}

	m.kbCursorLine, m.kbCursorCol = line, col
	if !m.kbSelecting {
		// Cursor-only sub-state: the anchor tracks the cursor so the
		// highlight stays a single-character "normal mode" cursor block
		// instead of growing into a selection.
		m.kbAnchorLine, m.kbAnchorCol = line, col
	}
	// Remember the cursor position so a later Esc-then-v (or a fresh
	// EnterVisual on the same item) resumes from here.
	m.cursorItem, m.cursorLine, m.cursorCol = m.kbVisualItem, line, col
	return true, nil
}

// YankVisual copies the current keyboard visual-mode selection to the
// clipboard and exits visual mode.
func (m *Chat) YankVisual() tea.Cmd {
	text := m.HighlightContent()
	return common.CopyToClipboardWithCallback(
		text,
		"Selection copied to clipboard",
		func() tea.Msg {
			m.ExitVisual()
			return nil
		},
	)
}

// clampInt clamps v to the inclusive range [lo, hi].
func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
