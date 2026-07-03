package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestChatHighlightContent_ResolvesWrappedCodeLineToOriginalSource is the
// end-to-end regression test for the bug report: a long single-line shell
// command inside a fenced code block gets hard-wrapped by glamour across
// several rendered rows. Selecting (via keyboard visual mode, exercising
// the same Chat.HighlightContent path a mouse drag would) any of those
// wrapped rows must copy back the original, unbroken command — not the
// wrapped fragments with an inserted newline.
func TestChatHighlightContent_ResolvesWrappedCodeLineToOriginalSource(t *testing.T) {
	t.Parallel()

	u := newTestUI()

	longCmd := `$env:PATH = [System.Environment]::GetEnvironmentVariable("PATH","Machine") + ";" + [System.Environment]::GetEnvironmentVariable("PATH","User")`
	msg := &message.Message{
		ID:   "m-fence",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Here's the command you need:\n\n```powershell\n" + longCmd + "\n```\n\nRun that in your shell."},
		},
	}
	item := chat.NewAssistantMessageItem(u.com.Styles, msg)

	u.chat.SetMessages(item)
	u.updateLayoutAndSize()
	u.chat.Focus()
	u.chat.SetSelected(0)

	listWidth := u.chat.list.Width()
	rendered := ansi.Strip(item.RawRender(listWidth))
	lines := strings.Split(rendered, "\n")

	// Locate the wrapped command inside the rendered item: find the
	// first line containing a recognizable fragment of it.
	fragment := "GetEnvironmentVariable"
	hitLine := -1
	for i, l := range lines {
		if strings.Contains(l, fragment) {
			hitLine = i
			break
		}
	}
	require.GreaterOrEqual(t, hitLine, 0, "rendered output must contain the wrapped command")

	// Drive a real keyboard visual-mode selection: enter visual mode,
	// move the cursor down to the fragment's first wrapped row, lock the
	// anchor, then extend a couple more rows to span the wrap.
	require.True(t, u.chat.EnterVisual(false))
	for range hitLine {
		_, _ = u.chat.HandleVisualKeyMsg(keyMsg("j"))
	}
	_, _ = u.chat.HandleVisualKeyMsg(keyMsg("v")) // lock the anchor here
	_, _ = u.chat.HandleVisualKeyMsg(keyMsg("j"))
	_, _ = u.chat.HandleVisualKeyMsg(keyMsg("j"))

	renderChat(t, u.chat)
	got := u.chat.HighlightContent()
	require.Equal(t, longCmd, got,
		"selecting the wrapped command's rendered rows must copy back the single original source line, not a broken multi-line fragment")
	require.NotContains(t, got, "\n", "the copied command must not contain an inserted line break")
}

// TestChatHighlightContent_ProseAroundFenceUnaffected verifies that
// selecting ordinary prose text (outside any fence) is unaffected by the
// fence-copy feature and still returns the rendered text as before.
func TestChatHighlightContent_ProseAroundFenceUnaffected(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	msg := &message.Message{
		ID:   "m-prose",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Here's the command you need:\n\n```powershell\necho hi\n```\n"},
		},
	}
	item := chat.NewAssistantMessageItem(u.com.Styles, msg)
	u.chat.SetMessages(item)
	u.updateLayoutAndSize()
	u.chat.Focus()
	u.chat.SetSelected(0)

	// Select just the first (prose) line: enter visual mode, lock at
	// (0,0), extend to the end of the line.
	require.True(t, u.chat.EnterVisual(false))
	_, _ = u.chat.HandleVisualKeyMsg(keyMsg("v"))
	_, _ = u.chat.HandleVisualKeyMsg(keyMsg("$"))

	renderChat(t, u.chat)
	got := u.chat.HighlightContent()
	require.Contains(t, got, "Here's the command you need")
}
