package hashline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Warnings surfaced during parsing/expansion.
const (
	bareBodyAutoPrefixedWarning = "Auto-prefixed bare body row(s) with `+`. Body rows must be `+TEXT` literal lines; `+` alone adds a blank line."
)

var (
	sectionHeaderRe = regexp.MustCompile(`^\[(.+)` + FileHashSep + `([0-9A-Fa-f]{` + strconv.Itoa(FileHashLength) + `})\]$`)
	// A run of digits, optionally a range with one of the accepted separators.
	rangeRe = regexp.MustCompile(`^([0-9]+)(?:\s*(?:\.=|\.\.|…|-|\s)\s*([0-9]+))?$`)
)

type opKind int

const (
	opSwap opKind = iota
	opDel
	opInsPre
	opInsPost
	opInsHead
	opInsTail
	opSwapBlk
	opDelBlk
	opInsBlk
)

// pendingOp is a parsed op header awaiting its body rows.
type pendingOp struct {
	kind    opKind
	start   int
	end     int
	body    bool // header ends in ":" (takes body rows)
	srcLine int
}

// Parse parses hashline patch text into file sections. It returns the sections,
// any non-fatal warnings, and a fatal error (with a 1-indexed patch line number
// where possible).
func Parse(input string) ([]Section, []string, error) {
	p := &parser{}
	return p.parse(input)
}

type parser struct {
	warnings []string
	sections []Section
	cur      *Section
	op       *pendingOp
	body     []string
}

func (p *parser) parse(input string) ([]Section, []string, error) {
	lines := strings.Split(input, "\n")
	seenNonBlank := false

	for idx, raw := range lines {
		lineNo := idx + 1
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)

		// Consume optional apply_patch envelope markers.
		if isEnvelopeMarker(trimmed) {
			if strings.EqualFold(trimmed, "*** Abort") {
				break
			}
			continue
		}
		if trimmed == "" {
			// Blank line: only meaningful as a blank body row when it carries
			// the payload sigil; a truly empty line is ignored between ops.
			if p.op != nil && p.op.body {
				// A bare blank line is not a body row; ignore it.
				continue
			}
			continue
		}
		if !seenNonBlank {
			seenNonBlank = true
			if _, _, ok := parseSectionHeader(trimmed); !ok {
				return nil, nil, fmt.Errorf(
					`input must begin with "[PATH%sHASH]" on the first non-blank line for anchored edits; got: %s`,
					FileHashSep, trimmed,
				)
			}
		}

		// Section header.
		if path, tag, ok := parseSectionHeader(trimmed); ok {
			if err := p.finishOp(); err != nil {
				return nil, nil, err
			}
			p.flushSection()
			p.cur = &Section{Path: path, Tag: strings.ToUpper(tag)}
			continue
		}

		if p.cur == nil {
			return nil, nil, fmt.Errorf("line %d: edit outside of any file section; expected a `[PATH%sHASH]` header first", lineNo, FileHashSep)
		}

		// Reject contamination early with actionable guidance.
		if err := rejectContamination(trimmed, lineNo); err != nil {
			return nil, nil, err
		}

		// File-level directives: REM (delete) and MV DEST (move/rename).
		if directive, ok, err := parseFileDirective(trimmed, lineNo); err != nil {
			return nil, nil, err
		} else if ok {
			if ferr := p.finishOp(); ferr != nil {
				return nil, nil, ferr
			}
			if directive.remove {
				p.cur.Remove = true
			} else {
				p.cur.MoveTo = directive.dest
			}
			continue
		}

		// Op header?
		if op, ok, err := parseOpHeader(trimmed, lineNo); err != nil {
			return nil, nil, err
		} else if ok {
			if err := p.finishOp(); err != nil {
				return nil, nil, err
			}
			p.op = &op
			p.body = nil
			continue
		}

		// Body row.
		if err := p.appendBody(trimmed, lineNo); err != nil {
			return nil, nil, err
		}
	}

	if err := p.finishOp(); err != nil {
		return nil, nil, err
	}
	p.flushSection()
	return p.sections, p.warnings, nil
}

func (p *parser) appendBody(line string, lineNo int) error {
	if p.op == nil || !p.op.body {
		return fmt.Errorf(
			"line %d: payload line has no preceding hunk header. Use `SWAP N%sM:`, `DEL N%sM`, or `INS.PRE|POST|HEAD|TAIL:` above the body. Got %q.",
			lineNo, RangeSep, RangeSep, line,
		)
	}
	switch {
	case strings.HasPrefix(line, PayloadSigil):
		p.body = append(p.body, line[len(PayloadSigil):])
	case strings.HasPrefix(line, "-"):
		return fmt.Errorf(
			"line %d: `-` rows are not valid; the range already names the lines being changed. For literal `-` lines (e.g. Markdown bullets), prefix the row with `+`: `+- item`.",
			lineNo,
		)
	default:
		// Bare body row: auto-prefix and warn.
		p.body = append(p.body, line)
		p.addWarningOnce(bareBodyAutoPrefixedWarning)
	}
	return nil
}

func (p *parser) addWarningOnce(w string) {
	for _, existing := range p.warnings {
		if existing == w {
			return
		}
	}
	p.warnings = append(p.warnings, w)
}

func (p *parser) flushSection() {
	if p.cur != nil {
		p.sections = append(p.sections, *p.cur)
		p.cur = nil
	}
}

// finishOp expands the pending op plus its collected body rows into concrete
// edits on the current section.
func (p *parser) finishOp() error {
	if p.op == nil {
		return nil
	}
	op := p.op
	body := p.body
	p.op = nil
	p.body = nil

	switch op.kind {
	case opSwap:
		if len(body) == 0 {
			// Empty SWAP is a deletion of the range.
			p.appendDeleteRange(op.start, op.end, op.srcLine)
			return nil
		}
		for _, text := range body {
			p.cur.Edits = append(p.cur.Edits, Edit{
				Kind:    EditInsert,
				Cursor:  Cursor{Kind: CursorBeforeAnchor, Line: op.start},
				Text:    text,
				Mode:    InsertReplacement,
				SrcLine: op.srcLine,
			})
		}
		p.appendDeleteRange(op.start, op.end, op.srcLine)
	case opDel:
		if len(body) > 0 {
			return fmt.Errorf("line %d: `DEL N%sM` does not take body rows. Remove the body, or use `SWAP N%sM:`.", op.srcLine, RangeSep, RangeSep)
		}
		p.appendDeleteRange(op.start, op.end, op.srcLine)
	case opInsPre:
		if err := requireBody(op, body); err != nil {
			return err
		}
		p.appendInserts(body, Cursor{Kind: CursorBeforeAnchor, Line: op.start}, op.srcLine)
	case opInsPost:
		if err := requireBody(op, body); err != nil {
			return err
		}
		p.appendInserts(body, Cursor{Kind: CursorAfterAnchor, Line: op.start}, op.srcLine)
	case opInsHead:
		if err := requireBody(op, body); err != nil {
			return err
		}
		p.appendInserts(body, Cursor{Kind: CursorBOF}, op.srcLine)
	case opInsTail:
		if err := requireBody(op, body); err != nil {
			return err
		}
		p.appendInserts(body, Cursor{Kind: CursorEOF}, op.srcLine)
	case opSwapBlk:
		if len(body) == 0 {
			return fmt.Errorf("line %d: `SWAP.BLK N:` needs at least one `+TEXT` body row. To delete a block, use `DEL.BLK N`.", op.srcLine)
		}
		p.cur.Edits = append(p.cur.Edits, Edit{
			Kind: EditBlock, BlockMode: BlockReplace, BlockLine: op.start, Payloads: body, SrcLine: op.srcLine,
		})
	case opDelBlk:
		if len(body) > 0 {
			return fmt.Errorf("line %d: `DEL.BLK N` does not take body rows. Remove the body, or use `SWAP.BLK N:`.", op.srcLine)
		}
		p.cur.Edits = append(p.cur.Edits, Edit{
			Kind: EditBlock, BlockMode: BlockDelete, BlockLine: op.start, SrcLine: op.srcLine,
		})
	case opInsBlk:
		if len(body) == 0 {
			return fmt.Errorf("line %d: `INS.BLK.POST N:` needs at least one `+TEXT` body row.", op.srcLine)
		}
		p.cur.Edits = append(p.cur.Edits, Edit{
			Kind: EditBlock, BlockMode: BlockInsertAfter, BlockLine: op.start, Payloads: body, SrcLine: op.srcLine,
		})
	}
	return nil
}

func requireBody(op *pendingOp, body []string) error {
	if len(body) == 0 {
		return fmt.Errorf("line %d: `INS` needs at least one `+TEXT` body row.", op.srcLine)
	}
	return nil
}

func (p *parser) appendInserts(body []string, cursor Cursor, srcLine int) {
	for _, text := range body {
		p.cur.Edits = append(p.cur.Edits, Edit{Kind: EditInsert, Cursor: cursor, Text: text, SrcLine: srcLine})
	}
}

func (p *parser) appendDeleteRange(start, end, srcLine int) {
	for line := start; line <= end; line++ {
		p.cur.Edits = append(p.cur.Edits, Edit{Kind: EditDelete, Anchor: line, SrcLine: srcLine})
	}
}

// parseSectionHeader recognizes "[PATH#TAG]".
func parseSectionHeader(line string) (path, tag string, ok bool) {
	m := sectionHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func isEnvelopeMarker(line string) bool {
	if !strings.HasPrefix(line, "***") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "***"))
	switch {
	case strings.EqualFold(rest, "Begin Patch"),
		strings.EqualFold(rest, "End Patch"),
		strings.EqualFold(rest, "Abort"):
		return true
	}
	return false
}

// rejectContamination catches apply_patch / unified-diff shapes with guidance.
func rejectContamination(line string, lineNo int) error {
	switch {
	case strings.HasPrefix(line, "@@"):
		return fmt.Errorf("line %d: `@@`-bracketed hunk header %q is not valid in hashline. Write a verb header such as `SWAP N%sM:`.", lineNo, line, RangeSep)
	case strings.HasPrefix(line, "*** Update File:"),
		strings.HasPrefix(line, "*** Add File:"),
		strings.HasPrefix(line, "*** Delete File:"),
		strings.HasPrefix(line, "*** Move to:"):
		return fmt.Errorf("line %d: apply_patch sentinel %q is not valid in hashline. File sections start with `[path%sHASH]`.", lineNo, line, FileHashSep)
	}
	return nil
}

// fileDirective is a parsed section-level REM/MV directive.
type fileDirective struct {
	remove bool
	dest   string
}

// parseFileDirective recognizes the section-level `REM` and `MV DEST`
// directives. DEST may be quoted when it contains spaces.
func parseFileDirective(line string, lineNo int) (fileDirective, bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return fileDirective{}, false, nil
	}
	switch strings.ToUpper(fields[0]) {
	case KeywordRemove:
		if len(fields) > 1 {
			return fileDirective{}, false, fmt.Errorf("line %d: `REM` deletes the section file and takes no arguments.", lineNo)
		}
		return fileDirective{remove: true}, true, nil
	case KeywordMove:
		dest := strings.TrimSpace(line[len(fields[0]):])
		dest = strings.Trim(dest, `"'`)
		if dest == "" {
			return fileDirective{}, false, fmt.Errorf("line %d: `MV` needs a destination path, e.g. `MV lib/greet.go`.", lineNo)
		}
		return fileDirective{dest: dest}, true, nil
	}
	return fileDirective{}, false, nil
}

// parseOpHeader recognizes an operation header line. It returns ok=false for a
// non-header line (a body row), and an error for a recognizable-but-invalid
// header.
func parseOpHeader(line string, lineNo int) (pendingOp, bool, error) {
	stripped := strings.TrimSuffix(line, HeaderColon)
	fields := strings.Fields(stripped)
	if len(fields) == 0 {
		return pendingOp{}, false, nil
	}
	keyword := strings.ToUpper(fields[0])
	args := strings.TrimSpace(stripped[len(fields[0]):])

	switch keyword {
	case KeywordSwapBlock:
		start, _, ok := parseRange(args)
		if !ok {
			return pendingOp{}, false, fmt.Errorf("line %d: `SWAP.BLK` needs a line number, e.g. `SWAP.BLK 12:`.", lineNo)
		}
		return pendingOp{kind: opSwapBlk, start: start, end: start, body: true, srcLine: lineNo}, true, nil
	case KeywordDeleteBlock:
		start, _, ok := parseRange(args)
		if !ok {
			return pendingOp{}, false, fmt.Errorf("line %d: `DEL.BLK` needs a line number, e.g. `DEL.BLK 12`.", lineNo)
		}
		return pendingOp{kind: opDelBlk, start: start, end: start, srcLine: lineNo}, true, nil
	case KeywordInsertBlock:
		start, _, ok := parseRange(args)
		if !ok {
			return pendingOp{}, false, fmt.Errorf("line %d: `INS.BLK.POST` needs a line number, e.g. `INS.BLK.POST 12:`.", lineNo)
		}
		return pendingOp{kind: opInsBlk, start: start, end: start, body: true, srcLine: lineNo}, true, nil
	case KeywordSwap:
		start, end, ok := parseRange(args)
		if !ok {
			return pendingOp{}, false, fmt.Errorf("line %d: `SWAP` needs a line or range, e.g. `SWAP 12%s14:`.", lineNo, RangeSep)
		}
		if end < start {
			return pendingOp{}, false, fmt.Errorf("line %d: range %d%s%d ends before it starts.", lineNo, start, RangeSep, end)
		}
		return pendingOp{kind: opSwap, start: start, end: end, body: true, srcLine: lineNo}, true, nil
	case KeywordDelete:
		start, end, ok := parseRange(args)
		if !ok {
			return pendingOp{}, false, fmt.Errorf("line %d: `DEL` needs a line or range, e.g. `DEL 12` or `DEL 12%s14`.", lineNo, RangeSep)
		}
		if end < start {
			return pendingOp{}, false, fmt.Errorf("line %d: range %d%s%d ends before it starts.", lineNo, start, RangeSep, end)
		}
		return pendingOp{kind: opDel, start: start, end: end, srcLine: lineNo}, true, nil
	}

	// INS.PRE / INS.POST / INS.HEAD / INS.TAIL (INS.BLK.POST handled above).
	if strings.HasPrefix(keyword, KeywordInsert+".") {
		return parseInsertHeader(keyword, args, lineNo)
	}

	// A bare numeric header ("12" or "12 14") without a verb.
	if _, _, ok := parseRange(stripped); ok {
		return pendingOp{}, false, fmt.Errorf("line %d: hunk headers need a verb. Use `SWAP N%sN:` to replace, or `DEL N` to delete.", lineNo, RangeSep)
	}
	return pendingOp{}, false, nil
}

func parseInsertHeader(keyword, args string, lineNo int) (pendingOp, bool, error) {
	switch keyword {
	case KeywordInsert + "." + InsertBefore:
		start, _, ok := parseRange(args)
		if !ok {
			return pendingOp{}, false, fmt.Errorf("line %d: `INS.PRE` needs a line number.", lineNo)
		}
		return pendingOp{kind: opInsPre, start: start, end: start, body: true, srcLine: lineNo}, true, nil
	case KeywordInsert + "." + InsertAfter:
		start, _, ok := parseRange(args)
		if !ok {
			return pendingOp{}, false, fmt.Errorf("line %d: `INS.POST` needs a line number.", lineNo)
		}
		return pendingOp{kind: opInsPost, start: start, end: start, body: true, srcLine: lineNo}, true, nil
	case KeywordInsert + "." + InsertHead:
		return pendingOp{kind: opInsHead, body: true, srcLine: lineNo}, true, nil
	case KeywordInsert + "." + InsertTail:
		return pendingOp{kind: opInsTail, body: true, srcLine: lineNo}, true, nil
	}
	return pendingOp{}, false, fmt.Errorf("line %d: unknown `INS` variant %q; use INS.PRE, INS.POST, INS.HEAD, or INS.TAIL.", lineNo, keyword)
}

// parseRange parses "N", "N.=M", "N-M", "N..M", "N…M", or "N M" and returns the
// start and end line numbers (end == start for a single line).
func parseRange(s string) (start, end int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	m := rangeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	start, err := strconv.Atoi(m[1])
	if err != nil || start < 1 {
		return 0, 0, false
	}
	end = start
	if m[2] != "" {
		end, err = strconv.Atoi(m[2])
		if err != nil || end < 1 {
			return 0, 0, false
		}
	}
	return start, end, true
}
