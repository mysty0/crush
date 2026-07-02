package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestBashToolStreamsPartialOutput verifies that a running (pending) bash
// tool item renders the command and any output streamed via
// SetPartialOutput before the final result arrives.
func TestBashToolStreamsPartialOutput(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	// The tool call is marked Finished as soon as its input is parsed —
	// before the command actually runs. Streaming must therefore work
	// even though Finished is true, as long as there is no result yet.
	toolCall := message.ToolCall{
		ID:       "tc-bash-1",
		Name:     "Bash",
		Input:    `{"command":"echo hello"}`,
		Finished: true,
	}
	item := NewBashToolMessageItem(&sty, toolCall, nil, false)

	setter, ok := any(item).(PartialOutputSetter)
	require.True(t, ok, "bash tool item must satisfy PartialOutputSetter")

	// Streamed partial output must appear while still running (no result),
	// along with the command header.
	setter.SetPartialOutput("partial line 1\npartial line 2")
	rendered := ansi.Strip(item.Render(80))
	require.Contains(t, rendered, "echo hello",
		"running bash item should show the command once output streams")
	require.Contains(t, rendered, "partial line 1",
		"streamed output must be visible while the command is running")
	require.Contains(t, rendered, "partial line 2")

	// The streaming view must not render a second "Bash" spinner line
	// beneath the output — that looked like a duplicate, stuck Bash item.
	require.Equal(t, 1, strings.Count(rendered, "Bash"),
		"streaming bash item must show exactly one Bash header, got:\n%s", rendered)

	// Once the final result is set, partial output no longer overrides it.
	item.SetResult(&message.ToolResult{
		ToolCallID: "tc-bash-1",
		Content:    "final output",
	})
	setter.SetPartialOutput("late partial that must be ignored")
	rendered = ansi.Strip(item.Render(80))
	require.Contains(t, rendered, "final output",
		"final result must be shown once available")
	require.NotContains(t, rendered, "late partial that must be ignored",
		"partial output must be ignored after the result arrives")
}
