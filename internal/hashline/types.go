package hashline

// CursorKind identifies where an insert lands relative to the file.
type CursorKind int

const (
	// CursorBeforeAnchor inserts immediately before Anchor.Line.
	CursorBeforeAnchor CursorKind = iota
	// CursorAfterAnchor inserts immediately after Anchor.Line.
	CursorAfterAnchor
	// CursorBOF inserts at the very start of the file.
	CursorBOF
	// CursorEOF inserts at the very end of the file.
	CursorEOF
)

// Cursor is an insert position.
type Cursor struct {
	Kind CursorKind
	Line int // valid for CursorBeforeAnchor / CursorAfterAnchor
}

// InsertMode distinguishes a plain insertion from the insert half of a SWAP
// (replacement) expansion. The distinction is only relevant to Phase 3
// boundary-repair heuristics; apply treats both identically.
type InsertMode int

const (
	// InsertPlain is a standalone insertion (INS.*).
	InsertPlain InsertMode = iota
	// InsertReplacement is the insert half of a SWAP expansion.
	InsertReplacement
)

// EditKind is the discriminator for Edit.
type EditKind int

const (
	// EditInsert inserts Text at Cursor.
	EditInsert EditKind = iota
	// EditDelete deletes the single line at Anchor.
	EditDelete
	// EditBlock is a deferred block op resolved later against file text via a
	// BlockResolver (SWAP.BLK / DEL.BLK / INS.BLK.POST).
	EditBlock
)

// BlockMode is the operation a deferred block edit expands into once its span
// is resolved.
type BlockMode int

const (
	// BlockReplace replaces the resolved block span with Payloads.
	BlockReplace BlockMode = iota
	// BlockDelete deletes the resolved block span (no payloads).
	BlockDelete
	// BlockInsertAfter inserts Payloads after the resolved block's last line.
	BlockInsertAfter
)

// Edit is a single parsed operation. Line-oriented ops (SWAP/DEL/INS.*) are
// parsed directly into EditInsert/EditDelete rows; block ops become a single
// EditBlock that resolveBlockEdits later expands into inserts/deletes.
type Edit struct {
	Kind EditKind

	// Insert fields (Kind == EditInsert).
	Cursor Cursor
	Text   string
	Mode   InsertMode

	// Delete fields (Kind == EditDelete).
	Anchor int // 1-indexed original line

	// Block fields (Kind == EditBlock).
	BlockMode BlockMode
	BlockLine int      // anchor line the block begins on
	Payloads  []string // replacement/insert body rows

	// SrcLine is the 1-indexed line in the patch input this edit came from,
	// used in error messages.
	SrcLine int
}

// BlockRequest is the input to a BlockResolver.
type BlockRequest struct {
	Path string // section file path (used to infer language)
	Text string // full current file text (LF-normalized)
	Line int    // 1-indexed line the block is expected to begin on
}

// BlockSpan is a resolved 1-indexed inclusive line span.
type BlockSpan struct {
	Start int
	End   int
}

// BlockResolver resolves the syntactic block that begins on a given line to
// its concrete line span. It returns ok=false when no block can be resolved
// (unsupported language, blank/closer line, no node beginning there, or a
// parse error). Implementations are injected so this package carries no
// tree-sitter dependency.
type BlockResolver interface {
	ResolveBlock(req BlockRequest) (BlockSpan, bool)
}

// BlockResolverFunc adapts a function to BlockResolver.
type BlockResolverFunc func(BlockRequest) (BlockSpan, bool)

// ResolveBlock implements BlockResolver.
func (f BlockResolverFunc) ResolveBlock(req BlockRequest) (BlockSpan, bool) {
	return f(req)
}

// Section is one parsed file section: a header path + tag and the edits under
// it. A section may also carry a file-level directive: Remove deletes the file,
// MoveTo renames/moves it (line edits, if any, apply to the source first, then
// the result is written at MoveTo).
type Section struct {
	Path   string
	Tag    string
	Edits  []Edit
	Remove bool   // REM: delete the section file
	MoveTo string // MV DEST: rename/move the section file to DEST
}
