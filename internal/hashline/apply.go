package hashline

import (
	"errors"
	"fmt"
	"strings"
)

// ApplyResult carries the post-edit text and any non-fatal warnings.
type ApplyResult struct {
	Text     string
	Warnings []string
}

// ErrNoChange indicates the edits applied cleanly but produced identical text.
var ErrNoChange = errors.New("hashline: edits produced no change")

// Apply applies a list of resolved edits (no EditBlock entries remain) to text
// and returns the new text. Line numbers in edits refer to the original text
// and never shift as edits apply.
//
// Text is assumed already normalized to LF. The trailing newline (if any) is
// preserved.
func Apply(text string, edits []Edit) (ApplyResult, error) {
	hadTrailingNL := strings.HasSuffix(text, "\n")
	core := strings.TrimSuffix(text, "\n")
	var lines []string
	if core != "" || (!hadTrailingNL && text != "") {
		lines = strings.Split(core, "\n")
	}
	n := len(lines)

	var (
		bof, eof []string
		before   = map[int][]string{}
		after    = map[int][]string{}
		deleted  = map[int]bool{}
	)

	for _, e := range edits {
		switch e.Kind {
		case EditBlock:
			return ApplyResult{}, fmt.Errorf("internal: unresolved block edit reached Apply (line %d)", e.SrcLine)
		case EditInsert:
			switch e.Cursor.Kind {
			case CursorBOF:
				bof = append(bof, e.Text)
			case CursorEOF:
				eof = append(eof, e.Text)
			case CursorBeforeAnchor:
				if err := checkLine(e.Cursor.Line, n); err != nil {
					return ApplyResult{}, err
				}
				before[e.Cursor.Line] = append(before[e.Cursor.Line], e.Text)
			case CursorAfterAnchor:
				if err := checkLine(e.Cursor.Line, n); err != nil {
					return ApplyResult{}, err
				}
				after[e.Cursor.Line] = append(after[e.Cursor.Line], e.Text)
			}
		case EditDelete:
			if err := checkLine(e.Anchor, n); err != nil {
				return ApplyResult{}, err
			}
			if deleted[e.Anchor] {
				return ApplyResult{}, fmt.Errorf(
					"line %d: line %d is already targeted by another hunk. Issue ONE hunk per range; the payload is only the final desired content.",
					e.SrcLine, e.Anchor,
				)
			}
			deleted[e.Anchor] = true
		}
	}

	out := make([]string, 0, n+len(bof)+len(eof))
	out = append(out, bof...)
	for i := 0; i < n; i++ {
		li := i + 1
		out = append(out, before[li]...)
		if !deleted[li] {
			out = append(out, lines[i])
		}
		out = append(out, after[li]...)
	}
	out = append(out, eof...)

	var result string
	if len(out) > 0 {
		result = strings.Join(out, "\n")
		if hadTrailingNL {
			result += "\n"
		}
	}

	if result == text {
		return ApplyResult{Text: result}, ErrNoChange
	}
	return ApplyResult{Text: result}, nil
}

func checkLine(line, n int) error {
	if line < 1 || line > n {
		return fmt.Errorf("line %d does not exist (file has %d lines)", line, n)
	}
	return nil
}
