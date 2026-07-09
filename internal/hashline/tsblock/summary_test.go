package tsblock

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// renderForTest produces a compact "kept lines + N-M elided" view for asserting.
func renderForTest(text string, s Summary) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	var b strings.Builder
	for _, seg := range s.Segments {
		if seg.Kind == Elided {
			b.WriteString("   … (")
			b.WriteString(itoa(seg.Start))
			b.WriteString("-")
			b.WriteString(itoa(seg.End))
			b.WriteString(")\n")
			continue
		}
		for ln := seg.Start; ln <= seg.End && ln <= len(lines); ln++ {
			b.WriteString(itoa(ln))
			b.WriteString(":")
			b.WriteString(lines[ln-1])
			b.WriteString("\n")
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestSummarizeGoFunction(t *testing.T) {
	t.Parallel()
	src := "package main\n\nfunc hello(name string) {\n\tx := 1\n\ty := 2\n\tprintln(x + y)\n\tprintln(name)\n}\n\nfunc small() {\n}\n"
	s, ok := Summarize(src, "main.go", Options{MinTotalLines: 1, UnfoldUntil: 1})
	require.True(t, ok)
	// The body of hello (lines 4-7) should be elided; signature (3) and closer (8) kept.
	require.NotZero(t, s.ElidedLines)
	view := renderForTest(src, s)
	require.Contains(t, view, "3:func hello(name string) {")
	require.Contains(t, view, "8:}")
	require.NotContains(t, view, "x := 1") // interior elided
	require.Contains(t, view, "…")
}

func TestSummarizeContainerKeepsMethodSignatures(t *testing.T) {
	t.Parallel()
	src := "package main\n\ntype T struct {\n\tA int\n\tB int\n\tC int\n}\n\nfunc (t T) One() int {\n\treturn t.A + t.B + t.C\n}\n\nfunc (t T) Two() int {\n\treturn t.A - t.B - t.C\n}\n"
	s, ok := Summarize(src, "main.go", Options{MinTotalLines: 1, UnfoldUntil: 1})
	require.True(t, ok)
	view := renderForTest(src, s)
	// Method signatures stay visible; their bodies are elided.
	require.Contains(t, view, "func (t T) One() int {")
	require.Contains(t, view, "func (t T) Two() int {")
}

func TestSummarizeTypeScript(t *testing.T) {
	t.Parallel()
	src := "export function add(a: number, b: number): number {\n\tconst sum = a + b;\n\tconsole.log(sum);\n\treturn sum;\n}\n"
	s, ok := Summarize(src, "a.ts", Options{MinTotalLines: 1, UnfoldUntil: 1})
	require.True(t, ok)
	view := renderForTest(src, s)
	require.Contains(t, view, "export function add(a: number, b: number): number {")
	require.NotContains(t, view, "const sum")
}

func TestSummarizeUnderBudgetShownWhole(t *testing.T) {
	t.Parallel()
	// A parseable file at or below the visible-line budget is shown verbatim:
	// ok=true, nothing elided, one kept segment covering every line.
	src := "package main\n\nfunc a() {\n\tx := 1\n\ty := 2\n\tz := 3\n}\n"
	s, ok := Summarize(src, "main.go", Options{MinTotalLines: 1, UnfoldUntil: 1000})
	require.True(t, ok)
	require.Zero(t, s.ElidedLines)
	require.Len(t, s.Segments, 1)
	require.Equal(t, Kept, s.Segments[0].Kind)
	require.Equal(t, s.TotalLines, s.Segments[0].End)
}

func TestSummarizeLargeUnfoldableFallsBack(t *testing.T) {
	t.Parallel()
	// A file larger than the budget with no foldable structure cannot be
	// collapsed, so the summarizer bails (ok=false) rather than dump it whole.
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < 40; i++ {
		b.WriteString("var x = 1\n")
	}
	_, ok := Summarize(b.String(), "main.go", Options{MinTotalLines: 1, UnfoldUntil: 10})
	require.False(t, ok)
}

func TestSummarizeUnknownLanguage(t *testing.T) {
	t.Parallel()
	_, ok := Summarize("some\nlong\nplain\ntext\nhere\n", "notes.unknownext", Options{MinTotalLines: 1})
	require.False(t, ok)
}

func TestSummarizeSegmentsCoverAllLines(t *testing.T) {
	t.Parallel()
	src := "package main\n\nfunc a() {\n\tx := 1\n\ty := 2\n\tz := 3\n}\n"
	s, ok := Summarize(src, "main.go", Options{MinTotalLines: 1, UnfoldUntil: 1})
	require.True(t, ok)
	// Segments must be gap-free and cover 1..TotalLines.
	next := 1
	for _, seg := range s.Segments {
		require.Equal(t, next, seg.Start)
		require.LessOrEqual(t, seg.Start, seg.End)
		next = seg.End + 1
	}
	require.Equal(t, s.TotalLines+1, next)
}
