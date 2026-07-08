package hashline

import "fmt"

// ResolveBlockEdits expands every deferred block edit against text (parsed as
// the language inferred from path) using resolver, returning a fresh edit list
// with no EditBlock entries. Non-block edits pass through untouched. It returns
// any lowering warnings.
//
// A replace/delete block that cannot be resolved is a fatal error. An
// unresolvable insert-after-block is lowered to a plain insert after the anchor
// line with a warning (it never fails the patch).
func ResolveBlockEdits(edits []Edit, text, path string, resolver BlockResolver) ([]Edit, []string, error) {
	if !hasBlockEdit(edits) {
		return edits, nil, nil
	}
	var (
		out      = make([]Edit, 0, len(edits))
		warnings []string
	)
	for _, e := range edits {
		if e.Kind != EditBlock {
			out = append(out, e)
			continue
		}

		span, ok := resolveSpan(resolver, path, text, e.BlockLine)
		if !ok || span.Start == span.End {
			// Unresolvable or single-line: insert-after lowers to a plain
			// insert; replace/delete are rejected.
			if e.BlockMode == BlockInsertAfter {
				anchor := e.BlockLine
				if ok {
					anchor = span.End
				}
				warnings = append(warnings, fmt.Sprintf(
					"line %d: `INS.BLK.POST %d` could not resolve a block; lowered to `INS.POST %d`. Verify the landing line.",
					e.SrcLine, e.BlockLine, anchor,
				))
				for _, text := range e.Payloads {
					out = append(out, Edit{Kind: EditInsert, Cursor: Cursor{Kind: CursorAfterAnchor, Line: anchor}, Text: text, SrcLine: e.SrcLine})
				}
				continue
			}
			return nil, nil, blockUnresolvedErr(e, ok, span)
		}

		switch e.BlockMode {
		case BlockReplace:
			for _, text := range e.Payloads {
				out = append(out, Edit{Kind: EditInsert, Cursor: Cursor{Kind: CursorBeforeAnchor, Line: span.Start}, Text: text, Mode: InsertReplacement, SrcLine: e.SrcLine})
			}
			for line := span.Start; line <= span.End; line++ {
				out = append(out, Edit{Kind: EditDelete, Anchor: line, SrcLine: e.SrcLine})
			}
		case BlockDelete:
			for line := span.Start; line <= span.End; line++ {
				out = append(out, Edit{Kind: EditDelete, Anchor: line, SrcLine: e.SrcLine})
			}
		case BlockInsertAfter:
			for _, text := range e.Payloads {
				out = append(out, Edit{Kind: EditInsert, Cursor: Cursor{Kind: CursorAfterAnchor, Line: span.End}, Text: text, SrcLine: e.SrcLine})
			}
		}
	}
	return out, warnings, nil
}

func resolveSpan(resolver BlockResolver, path, text string, line int) (BlockSpan, bool) {
	if resolver == nil {
		return BlockSpan{}, false
	}
	return resolver.ResolveBlock(BlockRequest{Path: path, Text: text, Line: line})
}

func blockUnresolvedErr(e Edit, resolved bool, span BlockSpan) error {
	verb := "SWAP.BLK"
	fallback := fmt.Sprintf("SWAP %d%sM", e.BlockLine, RangeSep)
	if e.BlockMode == BlockDelete {
		verb, fallback = "DEL.BLK", fmt.Sprintf("DEL %d%sM", e.BlockLine, RangeSep)
	}
	if resolved && span.Start == span.End {
		return fmt.Errorf(
			"line %d: `%s %d` resolved to a single line (%d) — that is a statement, not a multi-line block. Use `%s` with explicit lines.",
			e.SrcLine, verb, e.BlockLine, span.Start, fallback,
		)
	}
	return fmt.Errorf(
		"line %d: `%s %d` could not resolve a syntactic block beginning on line %d (unsupported language, blank/closer line, or parse error). Use `%s` with explicit lines.",
		e.SrcLine, verb, e.BlockLine, e.BlockLine, fallback,
	)
}

func hasBlockEdit(edits []Edit) bool {
	for _, e := range edits {
		if e.Kind == EditBlock {
			return true
		}
	}
	return false
}
