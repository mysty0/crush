package hashline

import (
	"strings"
	"unicode/utf8"
)

// DefaultFuzzyThreshold is the minimum normalized-similarity percentage
// (0-100) an anchor line must reach against a line in the live file before
// fuzzy recovery will relocate an edit onto it. It is deliberately high: fuzzy
// recovery is a last-chance fallback that runs only after exact recovery has
// failed, so it must not guess. Ported from oh-my-pi's fuzzy edit matcher,
// whose default similarity gate is 0.95.
const DefaultFuzzyThreshold = 90

// RecoverFuzzy attempts to salvage edits anchored against base when the exact
// 3-way Recover has already failed, by relocating each anchored edit onto the
// line in live whose whitespace/punctuation-normalized content matches the
// base anchor line.
//
// base is the snapshot version the edits were anchored to; live is the drifted
// on-disk content. It is intentionally conservative and never guesses:
//
//   - Every anchored line in edits must match EXACTLY ONE line in live at or
//     above threshold similarity. Zero matches (the anchor content is gone) or
//     two-or-more matches (ambiguous — the content is duplicated) abort the
//     whole recovery. This duplicate guard is what keeps it from landing an
//     edit on the wrong occurrence.
//   - All anchors must relocate by the SAME line delta, so a scattered or
//     inconsistent set of matches aborts too.
//
// It returns the relocated-and-applied text and true only when a single,
// unambiguous relocation is proven; otherwise it returns ok=false and the
// caller falls back to a clean stale-tag rejection.
func RecoverFuzzy(base, live string, edits []Edit, threshold int) (string, bool) {
	// Block edits must already be resolved; an unresolved block span cannot be
	// safely relocated.
	for _, e := range edits {
		if e.Kind == EditBlock {
			return "", false
		}
	}

	anchors := AnchorLines(edits)
	if len(anchors) == 0 {
		// Nothing content-anchored to verify (e.g. BOF/EOF-only inserts);
		// refuse rather than relocate blindly.
		return "", false
	}

	baseLines := contentLines(base)
	liveLines := contentLines(live)

	delta := 0
	haveDelta := false
	for _, anchor := range anchors {
		if anchor < 1 || anchor > len(baseLines) {
			return "", false
		}
		want := normalizeForFuzzy(baseLines[anchor-1])
		if want == "" {
			// A blank or whitespace-only anchor cannot be matched uniquely.
			return "", false
		}
		matchLine, ok := uniqueFuzzyMatch(liveLines, want, threshold)
		if !ok {
			return "", false
		}
		d := matchLine - anchor
		if !haveDelta {
			delta, haveDelta = d, true
			continue
		}
		if d != delta {
			// Anchors disagree on where the block moved; bail.
			return "", false
		}
	}

	relocated := relocateEdits(edits, delta)
	applied, err := Apply(live, relocated)
	if err != nil {
		return "", false
	}
	return applied.Text, true
}

// uniqueFuzzyMatch returns the 1-indexed line in lines whose normalized
// content matches want at or above threshold similarity, and true only when
// exactly one such line exists. want is assumed already normalized.
func uniqueFuzzyMatch(lines []string, want string, threshold int) (int, bool) {
	matchLine := 0
	matches := 0
	for i, line := range lines {
		if fuzzySimilarityPercent(want, normalizeForFuzzy(line)) >= threshold {
			matches++
			if matches > 1 {
				return 0, false
			}
			matchLine = i + 1
		}
	}
	if matches != 1 {
		return 0, false
	}
	return matchLine, true
}

// relocateEdits shifts every anchored line in edits by delta. Cursor-anchored
// inserts and range deletes move; BOF/EOF inserts are unaffected. It returns
// the input unchanged when delta is zero.
func relocateEdits(edits []Edit, delta int) []Edit {
	if delta == 0 {
		return edits
	}
	out := make([]Edit, len(edits))
	for i, e := range edits {
		switch e.Kind {
		case EditDelete:
			e.Anchor += delta
		case EditInsert:
			if e.Cursor.Kind == CursorBeforeAnchor || e.Cursor.Kind == CursorAfterAnchor {
				e.Cursor.Line += delta
			}
		}
		out[i] = e
	}
	return out
}

// contentLines splits text into the same 1-indexed line slice Apply uses, so a
// matched line number lines up with the edit line numbers Apply expects.
func contentLines(text string) []string {
	hadTrailingNL := strings.HasSuffix(text, "\n")
	core := strings.TrimSuffix(text, "\n")
	if core != "" || (!hadTrailingNL && text != "") {
		return strings.Split(core, "\n")
	}
	return nil
}

// normalizeForFuzzy canonicalizes a line for fuzzy comparison: it trims outer
// whitespace, collapses internal space/tab runs to a single space, and folds
// smart quotes and dashes onto their ASCII forms. Ported from oh-my-pi's
// normalizeForFuzzy.
func normalizeForFuzzy(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(trimmed))
	prevSpace := false
	for _, r := range trimmed {
		switch r {
		case '"', '\u201C', '\u201D', '\u201E', '\u201F', '\u00AB', '\u00BB':
			r = '"'
		case '\'', '\u2018', '\u2019', '\u201A', '\u201B', '`', '\u00B4':
			r = '\''
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2212':
			r = '-'
		}
		if r == ' ' || r == '\t' {
			if prevSpace {
				continue
			}
			prevSpace = true
			b.WriteByte(' ')
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// fuzzySimilarityPercent returns a 0-100 similarity score derived from the
// Levenshtein distance between a and b: 100*(1 - distance/maxRuneLen).
func fuzzySimilarityPercent(a, b string) int {
	if a == b {
		return 100
	}
	la := utf8.RuneCountInString(a)
	lb := utf8.RuneCountInString(b)
	maxLen := la
	if lb > maxLen {
		maxLen = lb
	}
	if maxLen == 0 {
		return 100
	}
	dist := levenshtein(a, b)
	return int(float64(maxLen-dist) / float64(maxLen) * 100)
}

// levenshtein returns the rune-wise edit distance between a and b.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	ar := []rune(a)
	br := []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}
