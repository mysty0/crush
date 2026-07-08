package hashline

import (
	"fmt"
	"strings"
)

// mismatchContext is the number of context lines shown on each side of an
// anchor in a stale-tag mismatch report.
const mismatchContext = 2

// MismatchError reports that a section's tag does not match the live file. It
// distinguishes a recognized-but-drifted tag (the file changed since it was
// read) from a never-recorded tag.
type MismatchError struct {
	Path       string
	SectionTag string
	LiveHash   string
	Recognized bool // the tag was minted by a prior read this session
	Context    string
}

func (e *MismatchError) Error() string {
	var b strings.Builder
	if e.Recognized {
		fmt.Fprintf(&b, "file %s changed since it was read: your anchors use tag %s but the file now hashes to %s. STOP and re-read before editing.",
			e.Path, e.SectionTag, e.LiveHash)
	} else {
		fmt.Fprintf(&b, "unknown snapshot tag %s for %s (file now hashes to %s); use the `[path#tag]` header from your latest read. To create a new file, use the write tool.",
			e.SectionTag, e.Path, e.LiveHash)
	}
	if e.Context != "" {
		b.WriteString("\n\n")
		b.WriteString(e.Context)
	}
	return b.String()
}

// NewMismatchError builds a MismatchError with anchor context drawn from the
// live text.
func NewMismatchError(path, sectionTag, liveText string, liveHash string, recognized bool, anchors []int) *MismatchError {
	return &MismatchError{
		Path:       path,
		SectionTag: sectionTag,
		LiveHash:   liveHash,
		Recognized: recognized,
		Context:    anchorContext(liveText, anchors),
	}
}

// anchorContext renders up to mismatchContext lines around each anchor, marking
// the anchor line with "*".
func anchorContext(text string, anchors []int) string {
	if len(anchors) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	shown := map[int]bool{}
	var b strings.Builder
	for _, anchor := range anchors {
		lo := anchor - mismatchContext
		hi := anchor + mismatchContext
		if lo < 1 {
			lo = 1
		}
		if hi > len(lines) {
			hi = len(lines)
		}
		for ln := lo; ln <= hi; ln++ {
			if shown[ln] {
				continue
			}
			shown[ln] = true
			marker := " "
			if ln == anchor {
				marker = "*"
			}
			fmt.Fprintf(&b, "%s%d:%s\n", marker, ln, lines[ln-1])
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// AnchorLines returns the sorted-ish set of original line numbers an edit list
// references, for mismatch context. Order follows edit order.
func AnchorLines(edits []Edit) []int {
	var out []int
	seen := map[int]bool{}
	add := func(line int) {
		if line >= 1 && !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	for _, e := range edits {
		switch e.Kind {
		case EditDelete:
			add(e.Anchor)
		case EditInsert:
			if e.Cursor.Kind == CursorBeforeAnchor || e.Cursor.Kind == CursorAfterAnchor {
				add(e.Cursor.Line)
			}
		case EditBlock:
			add(e.BlockLine)
		}
	}
	return out
}
