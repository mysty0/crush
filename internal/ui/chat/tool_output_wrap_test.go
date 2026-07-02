package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestToolOutputPlainContentLongLine verifies that a single line wider than
// the available width is truncated when collapsed but wrapped across multiple
// rows when expanded, so the full content becomes readable.
func TestToolOutputPlainContentLongLine(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	width := 20
	longLine := strings.Repeat("x", 100)

	collapsed := ansi.Strip(toolOutputPlainContent(&sty, longLine, width, false))
	require.Contains(t, collapsed, "…",
		"collapsed long line should be truncated with an ellipsis")
	require.Equal(t, 1, len(strings.Split(collapsed, "\n")),
		"collapsed long line should occupy a single row")

	expanded := ansi.Strip(toolOutputPlainContent(&sty, longLine, width, true))
	require.NotContains(t, expanded, "…",
		"expanded long line should not be truncated")
	require.Greater(t, len(strings.Split(expanded, "\n")), 1,
		"expanded long line should wrap onto multiple rows")
	require.Equal(t, longLine, strings.ReplaceAll(strings.ReplaceAll(expanded, "\n", ""), " ", ""),
		"expanded output should preserve the full line content")
}
