package model

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/clipperhouse/displaywidth"
)

// wordSpan is a display-column range [Start, End) for a token within a
// single (ansi-stripped) line. Columns are display-width based to match
// the terminal cell grid, consistent with findWordBoundaries.
type wordSpan struct {
	Start, End int
}

// runeWordClass classifies r for vim-style small-word ("w"/"b"/"e")
// segmentation: -1 for whitespace, 0 for keyword characters (letters,
// digits, underscore), 1 for everything else (punctuation/symbols). This
// matches vim's default 'iskeyword' behavior, so e.g. "foo.bar()" splits
// into "foo", ".", "bar", "(", ")" — useful for selecting code fragments.
func runeWordClass(r rune) int {
	switch {
	case unicode.IsSpace(r):
		return -1
	case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
		return 0
	default:
		return 1
	}
}

// smallWordSpans splits line into vim "word" tokens: maximal runs of the
// same character class (keyword vs. punctuation/symbol), with whitespace
// as a separator. This backs the `w`/`b`/`e` motions.
func smallWordSpans(line string) []wordSpan {
	var spans []wordSpan
	col := 0
	class := -1 // -1 = whitespace/none, 0 = keyword, 1 = punctuation
	start := 0
	g := displaywidth.StringGraphemes(line)
	for g.Next() {
		cluster := g.Value()
		w := g.Width()
		r, _ := utf8.DecodeRuneInString(cluster)
		c := runeWordClass(r)
		if c != class {
			if class >= 0 {
				spans = append(spans, wordSpan{Start: start, End: col})
			}
			start = col
			class = c
		}
		col += w
	}
	if class >= 0 {
		spans = append(spans, wordSpan{Start: start, End: col})
	}
	return spans
}

// bigWordSpans splits line into vim "WORD" tokens: maximal runs of
// non-whitespace, without punctuation classing. This backs the
// `W`/`B`/`E` motions.
func bigWordSpans(line string) []wordSpan {
	var spans []wordSpan
	col := 0
	inWord := false
	start := 0
	g := displaywidth.StringGraphemes(line)
	for g.Next() {
		cluster := g.Value()
		w := g.Width()
		isSpace := strings.TrimSpace(cluster) == ""
		switch {
		case isSpace && inWord:
			spans = append(spans, wordSpan{Start: start, End: col})
			inWord = false
		case !isSpace && !inWord:
			inWord = true
			start = col
		}
		col += w
	}
	if inWord {
		spans = append(spans, wordSpan{Start: start, End: col})
	}
	return spans
}

// wordSpansFor returns smallWordSpans or bigWordSpans depending on big.
func wordSpansFor(line string, big bool) []wordSpan {
	if big {
		return bigWordSpans(line)
	}
	return smallWordSpans(line)
}

// lineDisplayWidth returns the display width of an ansi-stripped line.
func lineDisplayWidth(line string) int {
	return displaywidth.String(line)
}

// lineLastCol returns the column of the last character on line (0 for an
// empty line), matching vim's `$` when the cursor must stay on-line.
func lineLastCol(line string) int {
	w := lineDisplayWidth(line)
	if w == 0 {
		return 0
	}
	return w - 1
}

// lineFirstNonBlankCol returns the column of the first non-blank
// character on line, or 0 if the line is empty or all blank. Matches
// vim's `^`.
func lineFirstNonBlankCol(line string) int {
	col := 0
	g := displaywidth.StringGraphemes(line)
	for g.Next() {
		if strings.TrimSpace(g.Value()) != "" {
			return col
		}
		col += g.Width()
	}
	return 0
}

// endOfBuffer returns the position of the last character of the last
// non-empty line in lines, used as the clamp target when a forward motion
// runs out of words.
func endOfBuffer(lines []string) (line, col int) {
	l := len(lines) - 1
	if l < 0 {
		return 0, 0
	}
	return l, lineLastCol(lines[l])
}

// wordForward returns the position of the start of the next word (vim `w`
// when big is false, `W` when true) after (line, col), searching forward
// into subsequent lines when the current line is exhausted. Fully blank
// lines are not treated as word stops (they are skipped when searching for
// the next word, though `j`/`k` can still reach them). If no further word
// exists, the cursor clamps to the last character of the buffer.
func wordForward(lines []string, line, col int, big bool) (int, int) {
	if line < 0 {
		line = 0
	}
	for l := line; l < len(lines); l++ {
		for _, s := range wordSpansFor(lines[l], big) {
			if l > line || s.Start > col {
				return l, s.Start
			}
		}
	}
	return endOfBuffer(lines)
}

// wordBackward returns the position of the start of the previous word
// (vim `b`/`B`) before (line, col), searching backward into prior lines
// when needed. If col is inside a word (not at its start), it returns
// that word's start rather than skipping past it, matching vim. Clamps to
// (0, 0) at the start of the buffer.
func wordBackward(lines []string, line, col int, big bool) (int, int) {
	if line >= len(lines) {
		line = len(lines) - 1
	}
	for l := line; l >= 0; l-- {
		spans := wordSpansFor(lines[l], big)
		for i := len(spans) - 1; i >= 0; i-- {
			s := spans[i]
			if l < line || s.Start < col {
				return l, s.Start
			}
		}
	}
	return 0, 0
}

// wordEnd returns the position of the end (last column) of the next word
// (vim `e`/`E`) after (line, col), always moving forward at least one
// column. Clamps to the last character of the buffer if no further word
// exists.
func wordEnd(lines []string, line, col int, big bool) (int, int) {
	if line < 0 {
		line = 0
	}
	for l := line; l < len(lines); l++ {
		for _, s := range wordSpansFor(lines[l], big) {
			last := s.End - 1
			if last < 0 {
				continue
			}
			if l > line || last > col {
				return l, last
			}
		}
	}
	return endOfBuffer(lines)
}
