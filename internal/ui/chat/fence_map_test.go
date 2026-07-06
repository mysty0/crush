package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// renderFullForTest renders md through the same shared renderer
// buildFenceMap uses internally, so tests exercise the exact same
// glamour configuration (chroma formatter, styles) as production.
func renderFullForTest(t *testing.T, sty *styles.Styles, width int, md string) []string {
	t.Helper()
	renderer := common.MarkdownRenderer(sty, width)
	mu := common.LockMarkdownRenderer(renderer)
	mu.Lock()
	out, err := renderer.Render(md)
	mu.Unlock()
	require.NoError(t, err)
	out = ansi.Strip(strings.TrimSuffix(out, "\n"))
	return strings.Split(out, "\n")
}

func TestParseFences(t *testing.T) {
	src := "prose\n\n```go\nline one\nline two\n```\n\nmore prose\n\n```\nbare fence\n```\n"
	fences := parseFences(src)
	require.Len(t, fences, 2)
	require.Equal(t, "go", fences[0].lang)
	require.Equal(t, []string{"line one", "line two"}, fences[0].lines)
	require.Equal(t, "", fences[1].lang)
	require.Equal(t, []string{"bare fence"}, fences[1].lines)
}

func TestParseFences_UnterminatedFenceIgnored(t *testing.T) {
	src := "prose\n\n```go\nstill streaming, no closing fence yet"
	fences := parseFences(src)
	require.Empty(t, fences, "an unterminated (still-streaming) fence must not be treated as complete")
}

func TestParseFences_EmptyFenceSkipped(t *testing.T) {
	src := "```go\n```\n"
	fences := parseFences(src)
	require.Empty(t, fences)
}

func TestBuildFenceMap_ResolvesWrappedLongLineToOriginalSource(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	const width = 78

	longCmd := `$env:PATH = [System.Environment]::GetEnvironmentVariable("PATH","Machine") + ";" + [System.Environment]::GetEnvironmentVariable("PATH","User")`
	source := "Here's the command you need:\n\n```powershell\n" + longCmd + "\n```\n\nRun that in your shell.\n"

	fullLines := renderFullForTest(t, &sty, width, source)
	fm := buildFenceMap(&sty, source, fullLines, width)
	require.NotNil(t, fm, "a message with one fence must produce a non-nil fence map")
	require.Len(t, fm.ranges, 1)

	r := fm.ranges[0]
	// The long command wraps across multiple rendered rows; every row in
	// the fence's range must resolve back to the single original line.
	lines, ok := fm.RawLinesFor(r.startLine, r.endLine)
	require.True(t, ok)
	require.Equal(t, []string{longCmd}, lines)

	// A sub-range entirely inside the fence (e.g. just the first wrapped
	// row) must still resolve to the whole original line, not a partial
	// wrapped fragment.
	lines, ok = fm.RawLinesFor(r.startLine, r.startLine)
	require.True(t, ok)
	require.Equal(t, []string{longCmd}, lines)
}

func TestBuildFenceMap_MultipleRawLinesInOneFence(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	const width = 78

	rawLines := []string{"first line", "second line", "third line"}
	source := "```text\n" + strings.Join(rawLines, "\n") + "\n```\n"

	fullLines := renderFullForTest(t, &sty, width, source)
	fm := buildFenceMap(&sty, source, fullLines, width)
	require.NotNil(t, fm)
	require.Len(t, fm.ranges, 1)
	r := fm.ranges[0]

	// A range covering only the middle raw line's rendered row resolves
	// to just that line.
	row := -1
	for i, rl := range r.rowToRawLine {
		if rl == 1 {
			row = i
			break
		}
	}
	require.GreaterOrEqual(t, row, 0)
	lines, ok := fm.RawLinesFor(r.startLine+row, r.startLine+row)
	require.True(t, ok)
	require.Equal(t, []string{"second line"}, lines)
}

func TestBuildFenceMap_ContentRowsMapToOwnLineNoTrailingMarginShift(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	const width = 78

	rawLines := []string{"line one", "line two", "line three", "line four"}
	source := "Here is code:\n\n```go\n" + strings.Join(rawLines, "\n") + "\n```\n\nDone.\n"

	fullLines := renderFullForTest(t, &sty, width, source)
	fm := buildFenceMap(&sty, source, fullLines, width)
	require.NotNil(t, fm)
	require.Len(t, fm.ranges, 1)

	// Regression: glamour's trailing code-block margin used to shift the
	// row→line mapping, so each source line resolved to the line above it
	// and a whole-block selection came out one line short. Find the
	// rendered row of each source line by its text and assert it resolves
	// to that same line.
	for _, raw := range rawLines {
		row := -1
		for i, full := range fullLines {
			if strings.TrimSpace(full) == raw {
				row = i
				break
			}
		}
		require.GreaterOrEqualf(t, row, 0, "row for %q not found", raw)
		lines, ok := fm.RawLinesFor(row, row)
		require.Truef(t, ok, "row %d (%q) did not resolve", row, raw)
		require.Equalf(t, []string{raw}, lines,
			"rendered row %d (%q) resolved to the wrong source line", row, raw)
	}

	// Selecting the whole visible block (first to last content row) must
	// return every source line, not one short.
	firstRow, lastRow := -1, -1
	for i, full := range fullLines {
		if strings.TrimSpace(full) == rawLines[0] {
			firstRow = i
		}
		if strings.TrimSpace(full) == rawLines[len(rawLines)-1] {
			lastRow = i
		}
	}
	lines, ok := fm.RawLinesFor(firstRow, lastRow)
	require.True(t, ok)
	require.Equal(t, rawLines, lines)
}

func TestBuildFenceMap_MixedSelectionOutsideFenceReturnsFalse(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	const width = 78

	source := "prose before\n\n```text\ncode line\n```\n\nprose after\n"
	fullLines := renderFullForTest(t, &sty, width, source)
	fm := buildFenceMap(&sty, source, fullLines, width)
	require.NotNil(t, fm)
	r := fm.ranges[0]

	// A range starting before the fence must not resolve.
	_, ok := fm.RawLinesFor(0, r.endLine)
	require.False(t, ok)

	// A range ending after the fence must not resolve.
	_, ok = fm.RawLinesFor(r.startLine, len(fullLines)-1)
	require.False(t, ok)
}

func TestBuildFenceMap_NoFencesReturnsNil(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	source := "just prose, no code blocks here\n"
	fullLines := renderFullForTest(t, &sty, 78, source)
	fm := buildFenceMap(&sty, source, fullLines, 78)
	require.Nil(t, fm)
}

func TestBuildFenceMap_MultipleFencesLocateInOrder(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	const width = 78

	source := "```text\nalpha\n```\n\nprose in between\n\n```text\nalpha\n```\n"
	fullLines := renderFullForTest(t, &sty, width, source)
	fm := buildFenceMap(&sty, source, fullLines, width)
	require.NotNil(t, fm)
	require.Len(t, fm.ranges, 2, "identical fences must each be located as a distinct, non-overlapping occurrence")
	require.Less(t, fm.ranges[0].endLine, fm.ranges[1].startLine, "the second fence must be located after the first")
}
