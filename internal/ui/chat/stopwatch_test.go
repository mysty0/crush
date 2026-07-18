package chat

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestToolStopwatchAppearsAfterThreshold verifies that a long-running tool
// call grows a live elapsed stopwatch on its spinner once the run passes
// spinnerStopwatchAfter, mirroring the assistant loader's behavior.
func TestToolStopwatchAppearsAfterThreshold(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	toolCall := message.ToolCall{
		ID:       "tc-bash-stopwatch",
		Name:     "Bash",
		Input:    `{"command":"sleep 60"}`,
		Finished: true,
	}
	item := NewBashToolMessageItem(&sty, toolCall, nil, false).(*BashToolMessageItem)

	// The anim's label fades in glyph-by-glyph over its ~1s birth
	// animation (see anim.maxBirthSteps); advance past it so the label is
	// fully settled before asserting on rendered text, matching what a
	// real render loop looks like a moment after the spinner appears.
	for range 25 {
		item.Advance()
	}

	// Fresh: hint present, no stopwatch digits yet (elapsed is wall-clock
	// time via startedAt, unaffected by the Advance() calls above).
	rendered := ansi.Strip(item.Render(80))
	require.Contains(t, rendered, "Running", "an immediate hint must show, not a bare spinner")
	require.NotRegexp(t, `\d+s`, rendered, "no stopwatch before the threshold")

	// Backdate the start time past the threshold and re-render.
	item.startedAt = time.Now().Add(-15 * time.Second)
	item.clearCache()
	rendered = ansi.Strip(item.Render(80))
	require.Regexp(t, `Running · \d+s`, rendered, "stopwatch must show a 'Running' hint, not bare digits")
}

// TestShellItemStopwatchAppearsAfterThreshold verifies the same live elapsed
// stopwatch for a pending bang-mode shell command.
func TestShellItemStopwatchAppearsAfterThreshold(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := NewPendingShellItem(&sty, "sleep 60")

	rendered := ansi.Strip(item.Render(80))
	require.Contains(t, rendered, "Running")
	require.NotRegexp(t, `Running · \d+s`, rendered, "no stopwatch before the threshold")

	item.startedAt = time.Now().Add(-15 * time.Second)
	item.clearCache()
	rendered = ansi.Strip(item.Render(80))
	require.Regexp(t, `Running · \d+s`, rendered, "stopwatch must appear once the run passes the threshold")
}

// TestAgentToolBareTrailingSpinnerGetsHint guards a specific gap: while a
// nested Agent/Agentic-Fetch tool is between sub-steps (no result yet, no
// nested tool call currently in flight either), its RenderTool renders the
// spinner completely bare -- no tool name, no adjacent text of any kind
// (see chat/agent.go). Before this fix, the only thing that could ever
// appear next to that spinner was raw elapsed digits with no word ("3m03s"),
// which read as noise next to the animation's decorative scrambled-glyph
// prefix. It must read as "Running · 3m03s".
func TestAgentToolBareTrailingSpinnerGetsHint(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	toolCall := message.ToolCall{
		ID:       "tc-agent-stopwatch",
		Name:     "Agent",
		Input:    `{"prompt":"investigate something"}`,
		Finished: true,
	}
	item := NewAgentToolMessageItem(&sty, toolCall, nil, false)
	// A nested tool must already exist so RenderTool takes the "still
	// running, show the trailing bare spinner" branch (chat/agent.go:193-196)
	// instead of the "no nested tools yet" pendingTool branch.
	item.AddNestedTool(NewBashToolMessageItem(&sty, message.ToolCall{
		ID: "tc-nested", Name: "Bash", Finished: true,
	}, &message.ToolResult{ToolCallID: "tc-nested", Content: "done"}, false))
	// The label fades in over the anim's ~1s birth animation (see
	// anim.maxBirthSteps); advance past it before asserting on rendered
	// text, matching what a real render loop looks like a moment after
	// the spinner appears.
	for range 25 {
		item.Advance()
	}

	// Immediate hint, before the stopwatch threshold: no adjacent header
	// text exists on this trailing-spinner line, so without an anim-level
	// default label this would render as bare scrambled glyphs.
	rendered := ansi.Strip(item.Render(80))
	require.Contains(t, rendered, "Running",
		"the bare trailing spinner must show an immediate hint, not just the scramble animation")

	item.startedAt = time.Now().Add(-15 * time.Second)
	item.clearCache()
	rendered = ansi.Strip(item.Render(80))
	require.Regexp(t, `Running · \d+s`, rendered,
		"the bare trailing spinner must show a word, not just elapsed digits")
}
