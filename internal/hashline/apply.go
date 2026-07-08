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

	// Absorb common off-by-one keeper mistakes where a SWAP payload restates an
	// unchanged line bordering the replaced range before validation runs.
	edits, repairWarnings := repairReplacementBoundaries(edits, lines)

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
		return ApplyResult{Text: result, Warnings: repairWarnings}, ErrNoChange
	}
	return ApplyResult{Text: result, Warnings: repairWarnings}, nil
}

func checkLine(line, n int) error {
	if line < 1 || line > n {
		return fmt.Errorf("line %d does not exist (file has %d lines)", line, n)
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// Replacement-boundary repair.
//
// Models routinely miscount a SWAP range's edges. A common mistake is to restate
// an UNCHANGED line that actually lives just OUTSIDE the range (an "off-by-one
// keeper"): the payload's trailing row duplicates the surviving line just past
// the range, or its leading row duplicates the surviving line just before it.
// Applied verbatim, that produces a duplicated boundary line.
//
// A balance-neutral boundary-echo repair absorbs this. It fires only when the
// dropped payload edge(s) are exact copies of the surviving line(s) outside the
// range AND removing them keeps delimiter balance intact, so intended duplicate
// statements survive while the common "payload restated the wrapper" mistake is
// repaired. This port covers the balance-neutral trailing keeper (the common
// case) and the balance-neutral two-sided echo; it deliberately omits the more
// speculative delimiter-imbalance closer repairs from the reference.

// replacementGroup is a run of replacement-mode inserts sharing one before
// anchor, immediately followed by the contiguous range deletes for that anchor.
// It mirrors how the parser lowers a `SWAP N.=M:` hunk with a body.
type replacementGroup struct {
	insertStart int      // Index of the first payload insert in the edit slice.
	insertEnd   int      // Index one past the last payload insert.
	deleteEnd   int      // Index of the last range delete in the edit slice.
	payload     []string // Payload body rows, in order.
	startLine   int      // First deleted line L (1-indexed).
	endLine     int      // Last deleted line M (1-indexed).
}

// findReplacementGroup detects a replacement group beginning at edits[start].
// It returns ok=false when the run there is not a SWAP body followed by its
// contiguous range deletes.
func findReplacementGroup(edits []Edit, start int) (replacementGroup, bool) {
	first := edits[start]
	if first.Kind != EditInsert || first.Mode != InsertReplacement ||
		first.Cursor.Kind != CursorBeforeAnchor {
		return replacementGroup{}, false
	}
	srcLine := first.SrcLine
	anchor := first.Cursor.Line

	i := start
	var payload []string
	for ; i < len(edits); i++ {
		e := edits[i]
		if e.Kind != EditInsert || e.Mode != InsertReplacement || e.SrcLine != srcLine {
			break
		}
		if e.Cursor.Kind != CursorBeforeAnchor || e.Cursor.Line != anchor {
			break
		}
		payload = append(payload, e.Text)
	}
	insertEnd := i

	expected := anchor
	for ; i < len(edits); i++ {
		e := edits[i]
		if e.Kind != EditDelete || e.SrcLine != srcLine || e.Anchor != expected {
			break
		}
		expected++
	}
	deleteCount := expected - anchor
	if deleteCount == 0 {
		return replacementGroup{}, false
	}
	return replacementGroup{
		insertStart: start,
		insertEnd:   insertEnd,
		deleteEnd:   i - 1,
		payload:     payload,
		startLine:   anchor,
		endLine:     anchor + deleteCount - 1,
	}, true
}

// repairReplacementBoundaries normalizes replacement groups so an off-by-one
// keeper does not duplicate an unchanged line bordering the range. It returns
// the repaired edit list plus one warning per repaired group. Non-group edits
// pass through untouched, and groups with no boundary echo are left unchanged.
func repairReplacementBoundaries(edits []Edit, fileLines []string) ([]Edit, []string) {
	out := make([]Edit, 0, len(edits))
	var warnings []string

	for i := 0; i < len(edits); {
		group, ok := findReplacementGroup(edits, i)
		if !ok {
			out = append(out, edits[i])
			i++
			continue
		}
		inserts := edits[group.insertStart:group.insertEnd]
		deletes := edits[group.insertEnd : group.deleteEnd+1]
		i = group.deleteEnd + 1

		leading, trailing, warn, repaired := boundaryEchoRepair(group, fileLines)
		if !repaired {
			out = append(out, inserts...)
			out = append(out, deletes...)
			continue
		}
		out = append(out, inserts[leading:len(inserts)-trailing]...)
		out = append(out, deletes...)
		warnings = append(warnings, warn)
	}
	return out, warnings
}

// boundaryEchoRepair decides how many payload rows to drop from each edge of a
// group. It returns the leading and trailing drop counts, a warning describing
// the repair, and whether any repair fired.
func boundaryEchoRepair(group replacementGroup, fileLines []string) (leading, trailing int, warning string, repaired bool) {
	// A repair only makes sense when the payload has room to spare a boundary
	// row; a single-row payload is the whole desired content.
	if len(group.payload) < 2 {
		return 0, 0, "", false
	}
	lead := countLeadingEcho(group, fileLines)
	trail := countTrailingEcho(group, fileLines)

	// Two-sided balance-neutral echo: both edges restate surviving lines and the
	// dropped rows do not disturb delimiter balance. Bail when the echoes would
	// claim the whole payload — that is indistinguishable from intended content.
	if lead > 0 && trail > 0 && lead+trail < len(group.payload) {
		leadBal := computeDelimiterBalance(group.payload[:lead])
		trailBal := computeDelimiterBalance(group.payload[len(group.payload)-trail:])
		dropped := balanceSum(leadBal, trailBal)
		if balanceIsZero(dropped) || balanceEqual(dropped, groupDelta(group, fileLines)) {
			return lead, trail, describeTwoSidedRepair(group, lead, trail), true
		}
	}

	// One-sided balance-neutral echo: fire only on a delimiter-balanced group so
	// the dropped edge is a keeper, not intentional structural content. Restrict
	// this to multi-line ranges (a construct rewrite): a single-line range is
	// riskier because an ordinary duplicated statement bordering it may be
	// intentional, and the two-sided path above already covers wrapper mistakes.
	if group.endLine == group.startLine {
		return 0, 0, "", false
	}
	if !balanceIsZero(groupDelta(group, fileLines)) {
		return 0, 0, "", false
	}
	// Exactly one side must echo; two-sided balanced echoes were handled above.
	if (lead > 0) == (trail > 0) {
		return 0, 0, "", false
	}
	if trail > 0 {
		if trail >= len(group.payload) {
			return 0, 0, "", false
		}
		if !balanceIsZero(computeDelimiterBalance(group.payload[len(group.payload)-trail:])) {
			return 0, 0, "", false
		}
		return 0, trail, describeTrailingRepair(group, trail), true
	}
	if lead >= len(group.payload) {
		return 0, 0, "", false
	}
	if !balanceIsZero(computeDelimiterBalance(group.payload[:lead])) {
		return 0, 0, "", false
	}
	return lead, 0, describeLeadingRepair(group, lead), true
}

// countTrailingEcho returns the largest k such that the payload's last k rows
// exactly equal the k surviving file lines just below the range (starting at
// M+1) and at least one echoed row carries non-whitespace content.
func countTrailingEcho(group replacementGroup, fileLines []string) int {
	maxK := min(len(group.payload), len(fileLines)-group.endLine)
	for k := maxK; k >= 1; k-- {
		matches := true
		hasContent := false
		for t := 0; t < k; t++ {
			line := group.payload[len(group.payload)-k+t]
			if line != fileLines[group.endLine+t] {
				matches = false
				break
			}
			hasContent = hasContent || strings.TrimSpace(line) != ""
		}
		if matches && hasContent {
			return k
		}
	}
	return 0
}

// countLeadingEcho returns the largest j such that the payload's first j rows
// exactly equal the j surviving file lines just above the range (ending at L-1)
// and at least one echoed row carries non-whitespace content.
func countLeadingEcho(group replacementGroup, fileLines []string) int {
	maxJ := min(len(group.payload), group.startLine-1)
	for j := maxJ; j >= 1; j-- {
		matches := true
		hasContent := false
		for t := 0; t < j; t++ {
			line := group.payload[t]
			if line != fileLines[group.startLine-1-j+t] {
				matches = false
				break
			}
			hasContent = hasContent || strings.TrimSpace(line) != ""
		}
		if matches && hasContent {
			return j
		}
	}
	return 0
}

// groupDelta is the delimiter balance the payload contributes minus the balance
// of the range it replaces.
func groupDelta(group replacementGroup, fileLines []string) delimBalance {
	rangeLines := fileLines[group.startLine-1 : group.endLine]
	return balanceSub(
		computeDelimiterBalance(group.payload),
		computeDelimiterBalance(rangeLines),
	)
}

func describeTrailingRepair(group replacementGroup, count int) string {
	return fmt.Sprintf(
		"Dropped %d off-by-one keeper row(s) that restated line %d, the surviving line just past the replaced range at line %d; issue the payload for the range only.",
		count, group.endLine+1, group.startLine,
	)
}

func describeLeadingRepair(group replacementGroup, count int) string {
	return fmt.Sprintf(
		"Dropped %d off-by-one keeper row(s) that restated line %d, the surviving line just before the replaced range at line %d; issue the payload for the range only.",
		count, group.startLine-1, group.startLine,
	)
}

func describeTwoSidedRepair(group replacementGroup, leading, trailing int) string {
	return fmt.Sprintf(
		"Dropped a replacement boundary echo at line %d: %d leading and %d trailing payload row(s) restated lines outside the range; issue the payload for the range only.",
		group.startLine, leading, trailing,
	)
}

// delimBalance is the net count of unmatched `()`, `[]`, and `{}` delimiters.
type delimBalance struct {
	paren, bracket, brace int
}

// computeDelimiterBalance returns the net delimiter delta across lines, skipping
// delimiters inside line comments (`//`), block comments, and string/template
// literals. Block-comment and backtick-template state carry across lines; `"`
// and `'` reset at each end-of-line since they cannot span lines. It is
// deliberately language-light: constructs it cannot classify are counted
// naively, which can only suppress a repair, never force one.
func computeDelimiterBalance(lines []string) delimBalance {
	var b delimBalance
	inBlockComment := false
	var quote byte
	for _, line := range lines {
		for i := 0; i < len(line); i++ {
			ch := line[i]
			if inBlockComment {
				if ch == '*' && i+1 < len(line) && line[i+1] == '/' {
					inBlockComment = false
					i++
				}
				continue
			}
			if quote != 0 {
				if ch == '\\' {
					i++
				} else if ch == quote {
					quote = 0
				}
				continue
			}
			switch {
			case ch == '"' || ch == '\'' || ch == '`':
				quote = ch
				continue
			case ch == '/' && i+1 < len(line) && line[i+1] == '/':
				i = len(line)
				continue
			case ch == '/' && i+1 < len(line) && line[i+1] == '*':
				inBlockComment = true
				i++
				continue
			}
			switch ch {
			case '(':
				b.paren++
			case ')':
				b.paren--
			case '[':
				b.bracket++
			case ']':
				b.bracket--
			case '{':
				b.brace++
			case '}':
				b.brace--
			}
		}
		// Single/double quotes cannot span lines; only backtick templates and
		// block comments do.
		if quote == '"' || quote == '\'' {
			quote = 0
		}
	}
	return b
}

func balanceSum(a, b delimBalance) delimBalance {
	return delimBalance{a.paren + b.paren, a.bracket + b.bracket, a.brace + b.brace}
}

func balanceSub(a, b delimBalance) delimBalance {
	return delimBalance{a.paren - b.paren, a.bracket - b.bracket, a.brace - b.brace}
}

func balanceIsZero(a delimBalance) bool {
	return a.paren == 0 && a.bracket == 0 && a.brace == 0
}

func balanceEqual(a, b delimBalance) bool {
	return a == b
}
