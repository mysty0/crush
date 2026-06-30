package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// streamReplay feeds content into a streamingMarkdown one growing
// prefix at a time (simulating streaming flushes), returning the
// output of the FINAL flush. Each flush renders the whole
// prefix-so-far, exactly as the live chat does on every new token
// batch.
//
// Stepping is by the supplied cut offsets, not per byte: glamour
// renders are expensive and per-byte stepping across the whole
// document does many redundant full renders. streamingMarkdown.Render
// acquires the per-renderer lock itself, so the caller must NOT also
// hold it (the lock is non-reentrant).
func streamReplay(t *testing.T, sty *styles.Styles, content string, width int, cuts []int) string {
	t.Helper()
	renderer := common.MarkdownRenderer(sty, width)

	var s streamingMarkdown
	var out string
	for _, end := range cuts {
		if end <= 0 || end > len(content) {
			continue
		}
		out = s.Render(content[:end], width, renderer)
	}
	out = s.Render(content, width, renderer)
	return out
}

// tableStreamCuts returns byte offsets to flush at. It flushes at
// every line end and — within the table region — at every byte, so
// the replay exercises each point at which the stable-prefix
// boundary can advance through the table. Per-byte stepping is
// confined to the (short) table so the test stays fast.
func tableStreamCuts(content string) []int {
	tableStart := strings.Index(content, "\n|")
	if tableStart < 0 {
		tableStart = len(content)
	}
	var cuts []int
	for i := 0; i < len(content); i++ {
		if i >= tableStart || content[i] == '\n' {
			cuts = append(cuts, i+1)
		}
	}
	return cuts
}

// TestStreamingTableMatchesOneShot replays a heading-then-table
// document through the streaming renderer and asserts the final
// streamed frame is visually identical to a single one-shot render.
//
// This guards the table path through streamingMarkdown: the boundary
// detector must keep the whole table on one side of the cut so the
// table is always rendered as a single glamour document (glamour
// computes column widths from the entire table, so a split would
// misalign the columns). The bug report that motivated this test was
// a comparison table whose columns shattered while streaming.
func TestStreamingTableMatchesOneShot(t *testing.T) {
	t.Parallel()

	// Heading + blank line + table: the real shape from the bug
	// report (a section heading followed by a table).
	content := strings.Join([]string{
		"### Web / research",
		"",
		"| Capability | Crush | Claude Code |",
		"|---|---|---|",
		"| Web fetch | Yes (fetch on coder agent) plus web_fetch sub-agent | Yes (WebFetch) |",
		"| Web search | Partial: exists but NOT on the main coder agent, uses DuckDuckGo scraping, only wired into the internal agentic_fetch sub-agent | Yes (native WebSearch) |",
		"| Sourcegraph code search | Yes (Crush-only, searches public GitHub) | No |",
		"| Deep research | No | Yes (research/insights flows) |",
	}, "\n")

	sty := styles.CharmtonePantera()
	cuts := tableStreamCuts(content)

	// Sweep widths so the test also catches width-specific column
	// wrapping differences between the streamed and one-shot paths.
	for _, width := range []int{40, 60, 78, 100, 120} {
		want := normalizeRender(freshRender(t, content, width))
		streamed := streamReplay(t, &sty, content, width, cuts)
		require.Equal(t, want, normalizeRender(streamed),
			"streamed render must match one-shot render at width=%d", width)
	}
}
