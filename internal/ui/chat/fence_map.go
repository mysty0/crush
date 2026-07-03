package chat

import (
	"strings"

	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

// FenceCopyable is implemented by message items that can resolve a
// highlighted line range back to the original (unwrapped) source lines
// when that range falls entirely within a single fenced code block.
//
// Glamour hard-wraps long code-block lines for display, inserting real
// newlines into the rendered output; a highlight/copy over those wrapped
// rows would otherwise copy the broken multi-line text instead of the
// original single-line command. Chat.HighlightContent prefers this
// method over extracting from the rendered buffer whenever it succeeds.
type FenceCopyable interface {
	// RawLinesForRange returns the original source lines corresponding to
	// rendered lines [startLine, endLine] (inclusive, both relative to the
	// item's own RawRender output) at the given render width. ok is false
	// if the range is not entirely contained within one fenced code block
	// — the caller should fall back to its default extraction in that
	// case.
	RawLinesForRange(width, startLine, endLine int) (lines []string, ok bool)
}

// rawFence is a single fenced code block as it appears in the raw
// (unrendered) markdown source.
type rawFence struct {
	lang  string
	lines []string
}

// fenceRange records where one fence ended up in an item's rendered
// output and how each of its rendered rows maps back to a raw source
// line.
type fenceRange struct {
	// startLine/endLine are the inclusive absolute line indices (within
	// the item's full RawRender output) spanned by this fence, including
	// glamour's leading margin row.
	startLine, endLine int
	// rowToRawLine[r] is the raw line index (into rawLines) that produced
	// rendered row r, where r is relative to startLine. The leading
	// margin row maps to raw line 0.
	rowToRawLine []int
	rawLines     []string
}

// fenceMap resolves rendered line ranges within one message item back to
// original source lines, for every fenced code block found in that
// item's raw markdown.
type fenceMap struct {
	ranges []fenceRange
}

// RawLinesFor returns the original source lines spanned by rendered rows
// [startLine, endLine] if — and only if — that whole range falls inside a
// single tracked fence. Column position is intentionally ignored: any
// selection touching part of a fence's rendered rows resolves to the
// complete raw lines those rows belong to, since a hard-wrapped source
// line should always be copied back whole.
func (fm *fenceMap) RawLinesFor(startLine, endLine int) ([]string, bool) {
	if fm == nil || startLine < 0 || endLine < 0 || startLine > endLine {
		return nil, false
	}
	for _, r := range fm.ranges {
		if startLine < r.startLine || endLine > r.endLine {
			continue
		}
		row0 := startLine - r.startLine
		row1 := endLine - r.startLine
		if row0 < 0 || row1 >= len(r.rowToRawLine) {
			return nil, false
		}
		rawStart := r.rowToRawLine[row0]
		rawEnd := r.rowToRawLine[row1]
		if rawStart < 0 || rawEnd < rawStart || rawEnd >= len(r.rawLines) {
			return nil, false
		}
		return r.rawLines[rawStart : rawEnd+1], true
	}
	return nil, false
}

// parseFences scans raw markdown source for fenced code blocks delimited
// by a line beginning with "```" (optionally followed by a language tag)
// and a later bare "```" line. Unterminated trailing fences (e.g. a
// still-streaming message) are ignored. Empty fences are skipped.
func parseFences(source string) []rawFence {
	lines := strings.Split(source, "\n")
	var fences []rawFence
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "```") {
			i++
			continue
		}
		lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
		i++
		start := i
		for i < len(lines) && strings.TrimSpace(lines[i]) != "```" {
			i++
		}
		if i >= len(lines) {
			break // unterminated fence
		}
		if body := lines[start:i]; len(body) > 0 {
			fences = append(fences, rawFence{lang: lang, lines: append([]string(nil), body...)})
		}
		i++ // skip the closing fence line
	}
	return fences
}

// fenceMarkdown reassembles a fence's markdown source from its language
// tag and body lines.
func fenceMarkdown(lang string, lines []string) string {
	return "```" + lang + "\n" + strings.Join(lines, "\n") + "\n```\n"
}

// renderIsolated renders md through the shared markdown renderer at
// width and returns its plain (ANSI-stripped) lines, with any purely
// blank leading/trailing lines trimmed. Rendering a fragment in
// isolation like this produces byte-identical output to that same
// fragment rendered as part of a larger document, which is what makes
// locating a fence within a full render reliable.
func renderIsolated(sty *styles.Styles, width int, md string) []string {
	renderer := common.MarkdownRenderer(sty, width)
	mu := common.LockMarkdownRenderer(renderer)
	mu.Lock()
	out, err := renderer.Render(md)
	mu.Unlock()
	if err != nil {
		return nil
	}
	out = ansi.Strip(strings.TrimSuffix(out, "\n"))
	out = strings.Trim(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// findContiguousRun returns the offset of the first occurrence of needle
// as a contiguous run within haystack, searching from index from
// onward, comparing lines with trailing spaces ignored (glamour
// right-pads lines to the render width). Returns -1 if not found.
func findContiguousRun(haystack, needle []string, from int) int {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return -1
	}
	for start := from; start+len(needle) <= len(haystack); start++ {
		match := true
		for j, nl := range needle {
			if strings.TrimRight(haystack[start+j], " ") != strings.TrimRight(nl, " ") {
				match = false
				break
			}
		}
		if match {
			return start
		}
	}
	return -1
}

// buildRowToRawLine determines, for a single fence, how many rendered
// rows each raw source line expands to. It renders successively longer
// prefixes of the fence's raw lines and diffs their total row counts;
// the delta after adding raw line k is attributed to that line (the
// leading margin row is attributed to raw line 0, since it's produced
// before any content line is rendered). Returns nil if rendering fails
// or produces a nonsensical (shrinking) count, in which case the caller
// should skip this fence rather than risk a wrong mapping.
func buildRowToRawLine(sty *styles.Styles, width int, f rawFence) []int {
	var rowToRaw []int
	prevTotal := 0
	for k := 1; k <= len(f.lines); k++ {
		rows := renderIsolated(sty, width, fenceMarkdown(f.lang, f.lines[:k]))
		total := len(rows)
		if total < prevTotal {
			return nil
		}
		for range total - prevTotal {
			rowToRaw = append(rowToRaw, k-1)
		}
		prevTotal = total
	}
	return rowToRaw
}

// buildFenceMap parses every fenced code block out of source, locates
// each one's rendered rows within fullLines (the item's full RawRender
// output, ANSI-stripped and split into lines), and builds a
// row-to-raw-line mapping for each. Fences that cannot be confidently
// located or mapped are skipped rather than risking an incorrect copy.
// Returns nil if source has no fences or none could be mapped.
func buildFenceMap(sty *styles.Styles, source string, fullLines []string, width int) *fenceMap {
	if !strings.Contains(source, "```") {
		return nil
	}
	fences := parseFences(source)
	if len(fences) == 0 {
		return nil
	}

	fm := &fenceMap{}
	searchFrom := 0
	for _, f := range fences {
		isolated := renderIsolated(sty, width, fenceMarkdown(f.lang, f.lines))
		if len(isolated) == 0 {
			continue
		}
		loc := findContiguousRun(fullLines, isolated, searchFrom)
		if loc < 0 {
			continue
		}
		searchFrom = loc + len(isolated)

		rowToRaw := buildRowToRawLine(sty, width, f)
		if len(rowToRaw) != len(isolated) {
			continue
		}

		fm.ranges = append(fm.ranges, fenceRange{
			startLine:    loc,
			endLine:      loc + len(isolated) - 1,
			rowToRawLine: rowToRaw,
			rawLines:     f.lines,
		})
	}
	if len(fm.ranges) == 0 {
		return nil
	}
	return fm
}
