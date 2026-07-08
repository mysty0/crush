package hashline

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComputeFileHashStableAcrossLineEndingsAndTrailingWS(t *testing.T) {
	t.Parallel()
	base := "a\nb\nc\n"
	require.Equal(t, ComputeFileHash(base), ComputeFileHash("a\r\nb\r\nc\r\n"), "CRLF must not change the tag")
	require.Equal(t, ComputeFileHash(base), ComputeFileHash("a  \nb\t\nc \n"), "trailing whitespace must not change the tag")
	require.Equal(t, ComputeFileHash(base), ComputeFileHash("\uFEFFa\nb\nc\n"), "leading BOM must not change the tag")
	require.NotEqual(t, ComputeFileHash(base), ComputeFileHash("a\nb\nd\n"), "different content must change the tag")
}

func TestComputeFileHashFormat(t *testing.T) {
	t.Parallel()
	tag := ComputeFileHash("hello world")
	require.Len(t, tag, FileHashLength)
	require.Equal(t, strings.ToUpper(tag), tag, "tag must be uppercase hex")
}

// applyPatch is a test helper: parse a single-section patch and apply it to
// text with no block resolver.
func applyPatch(t *testing.T, text, patch string) (string, []string) {
	t.Helper()
	sections, warns, err := Parse(patch)
	require.NoError(t, err)
	require.Len(t, sections, 1)
	edits, blockWarns, err := ResolveBlockEdits(sections[0].Edits, text, sections[0].Path, nil)
	require.NoError(t, err)
	res, err := Apply(text, edits)
	require.NoError(t, err)
	return res.Text, append(warns, append(blockWarns, res.Warnings...)...)
}

func TestApplySwapSingleLine(t *testing.T) {
	t.Parallel()
	text := "const X = \"a\";\nconst Y = X;\nexport { X, Y };\n"
	got, _ := applyPatch(t, text, "[a.ts#0000]\nSWAP 1.=1:\n+const X = \"b\";\n+export const Y = X;")
	require.Equal(t, "const X = \"b\";\nexport const Y = X;\nconst Y = X;\nexport { X, Y };\n", got)
}

func TestApplyInsertPostAndPre(t *testing.T) {
	t.Parallel()
	text := "line1\nline2\nline3\n"
	got, _ := applyPatch(t, text, "[f#0000]\nINS.POST 2:\n+after2")
	require.Equal(t, "line1\nline2\nafter2\nline3\n", got)

	got, _ = applyPatch(t, text, "[f#0000]\nINS.PRE 2:\n+before2")
	require.Equal(t, "line1\nbefore2\nline2\nline3\n", got)
}

func TestApplyInsertHeadTail(t *testing.T) {
	t.Parallel()
	text := "a\nb\n"
	got, _ := applyPatch(t, text, "[f#0000]\nINS.HEAD:\n+// header\nINS.TAIL:\n+// trailer")
	require.Equal(t, "// header\na\nb\n// trailer\n", got)
}

func TestApplyDeleteRange(t *testing.T) {
	t.Parallel()
	text := "a\nb\nc\nd\n"
	got, _ := applyPatch(t, text, "[f#0000]\nDEL 2.=3")
	require.Equal(t, "a\nd\n", got)
}

func TestApplyDeleteSingleLenient(t *testing.T) {
	t.Parallel()
	text := "a\nb\nc\n"
	got, _ := applyPatch(t, text, "[f#0000]\nDEL 2")
	require.Equal(t, "a\nc\n", got)
}

func TestApplyPreservesNoTrailingNewline(t *testing.T) {
	t.Parallel()
	text := "a\nb"
	got, _ := applyPatch(t, text, "[f#0000]\nSWAP 2.=2:\n+B")
	require.Equal(t, "a\nB", got)
}

func TestApplyEmptySwapIsDelete(t *testing.T) {
	t.Parallel()
	text := "a\nb\nc\n"
	got, _ := applyPatch(t, text, "[f#0000]\nSWAP 2.=2:")
	require.Equal(t, "a\nc\n", got)
}

func TestLenientRangeSeparators(t *testing.T) {
	t.Parallel()
	text := "a\nb\nc\nd\n"
	for _, header := range []string{"SWAP 2-3:", "SWAP 2..3:", "SWAP 2 3:", "SWAP 2.=3:"} {
		got, _ := applyPatch(t, text, "[f#0000]\n"+header+"\n+X")
		require.Equalf(t, "a\nX\nd\n", got, "header %q", header)
	}
}

func TestBareBodyRowWarns(t *testing.T) {
	t.Parallel()
	text := "a\nb\n"
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 1.=1:\nbare line")
	require.Equal(t, "bare line\nb\n", got)
	require.Contains(t, strings.Join(warns, "\n"), "Auto-prefixed")
}

func TestMinusRowRejected(t *testing.T) {
	t.Parallel()
	_, _, err := Parse("[f#0000]\nSWAP 1.=1:\n-old")
	require.Error(t, err)
	require.Contains(t, err.Error(), "`-` rows")
}

func TestBlankBodyRowViaPlusAlone(t *testing.T) {
	t.Parallel()
	text := "a\nb\n"
	got, _ := applyPatch(t, text, "[f#0000]\nINS.POST 1:\n+\n+c")
	require.Equal(t, "a\n\nc\nb\n", got)
}

func TestMissingSectionHeader(t *testing.T) {
	t.Parallel()
	_, _, err := Parse("SWAP 1.=1:\n+x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must begin with")
}

func TestContaminationRejected(t *testing.T) {
	t.Parallel()
	_, _, err := Parse("[f#0000]\n@@ -1,2 +1,2 @@\n+x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "@@")

	_, _, err = Parse("[f#0000]\n*** Update File: foo\n+x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "apply_patch")
}

func TestEnvelopeStripped(t *testing.T) {
	t.Parallel()
	text := "a\nb\n"
	got, _ := applyPatch(t, text, "*** Begin Patch\n[f#0000]\nSWAP 1.=1:\n+A\n*** End Patch")
	require.Equal(t, "A\nb\n", got)
}

func TestApplyOutOfRange(t *testing.T) {
	t.Parallel()
	sections, _, err := Parse("[f#0000]\nSWAP 9.=9:\n+x")
	require.NoError(t, err)
	_, err = Apply("a\nb\n", sections[0].Edits)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")
}

func TestApplyNoChangeIsError(t *testing.T) {
	t.Parallel()
	sections, _, err := Parse("[f#0000]\nSWAP 1.=1:\n+a")
	require.NoError(t, err)
	_, err = Apply("a\nb\n", sections[0].Edits)
	require.ErrorIs(t, err, ErrNoChange)
}

func TestOverlappingHunksRejected(t *testing.T) {
	t.Parallel()
	sections, _, err := Parse("[f#0000]\nDEL 2.=2\nDEL 2.=2")
	require.NoError(t, err)
	_, err = Apply("a\nb\nc\n", sections[0].Edits)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already targeted")
}

func TestMultiSection(t *testing.T) {
	t.Parallel()
	sections, _, err := Parse("[a.ts#0000]\nSWAP 1.=1:\n+x\n[b.ts#1111]\nDEL 2")
	require.NoError(t, err)
	require.Len(t, sections, 2)
	require.Equal(t, "a.ts", sections[0].Path)
	require.Equal(t, "0000", sections[0].Tag)
	require.Equal(t, "b.ts", sections[1].Path)
	require.Equal(t, "1111", sections[1].Tag)
}

func TestBlockOpWithoutResolverErrors(t *testing.T) {
	t.Parallel()
	sections, _, err := Parse("[f.go#0000]\nSWAP.BLK 1:\n+func x(){}")
	require.NoError(t, err)
	_, _, err = ResolveBlockEdits(sections[0].Edits, "func y(){}\n", "f.go", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "could not resolve")
}

func TestBlockInsertAfterLowersWithoutResolver(t *testing.T) {
	t.Parallel()
	sections, _, err := Parse("[f.go#0000]\nINS.BLK.POST 1:\n+// after")
	require.NoError(t, err)
	edits, warns, err := ResolveBlockEdits(sections[0].Edits, "a\nb\n", "f.go", nil)
	require.NoError(t, err)
	require.Contains(t, strings.Join(warns, "\n"), "lowered to `INS.POST")
	res, err := Apply("a\nb\n", edits)
	require.NoError(t, err)
	require.Equal(t, "a\n// after\nb\n", res.Text)
}

func TestStoreRecordAndByHash(t *testing.T) {
	t.Parallel()
	s := NewStore()
	tag := s.Record("sess", "f.go", "a\nb\n", []int{1, 2})
	snap, ok := s.ByHash("sess", "f.go", tag)
	require.True(t, ok)
	require.Equal(t, "a\nb\n", snap.Text)
	require.Contains(t, snap.SeenLines, 1)

	// Re-record identical content reuses the tag and fuses seen lines.
	tag2 := s.Record("sess", "f.go", "a\nb\n", []int{3})
	require.Equal(t, tag, tag2)
	snap, _ = s.Head("sess", "f.go")
	require.Contains(t, snap.SeenLines, 3)
}

func TestStoreRelocateAndInvalidate(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Record("sess", "old.go", "x\n", nil)
	s.Relocate("sess", "old.go", "new.go")
	_, ok := s.Head("sess", "old.go")
	require.False(t, ok)
	_, ok = s.Head("sess", "new.go")
	require.True(t, ok)
	s.Invalidate("sess", "new.go")
	_, ok = s.Head("sess", "new.go")
	require.False(t, ok)
}

func TestParseRemDirective(t *testing.T) {
	t.Parallel()
	secs, _, err := Parse("[f.go#0000]\nREM")
	require.NoError(t, err)
	require.True(t, secs[0].Remove)
	require.Empty(t, secs[0].Edits)
}

func TestParseMoveDirective(t *testing.T) {
	t.Parallel()
	secs, _, err := Parse("[f.go#0000]\nMV lib/f.go")
	require.NoError(t, err)
	require.Equal(t, "lib/f.go", secs[0].MoveTo)

	secs, _, err = Parse("[f.go#0000]\nSWAP 1.=1:\n+x\nMV lib/f.go")
	require.NoError(t, err)
	require.Equal(t, "lib/f.go", secs[0].MoveTo)
	require.Len(t, secs[0].Edits, 2) // insert + delete
}

func TestParseMoveQuotedDest(t *testing.T) {
	t.Parallel()
	secs, _, err := Parse(`[f.go#0000]` + "\n" + `MV "my dir/f.go"`)
	require.NoError(t, err)
	require.Equal(t, "my dir/f.go", secs[0].MoveTo)
}

func TestRecoverNonConflictingDrift(t *testing.T) {
	t.Parallel()
	base := "line1\nline2\nline3\nline4\n"
	// Live file drifted: line1 changed on disk (unrelated to our edit).
	live := "LINE1\nline2\nline3\nline4\n"
	// Our edit targets line3.
	edits := []Edit{
		{Kind: EditInsert, Cursor: Cursor{Kind: CursorBeforeAnchor, Line: 3}, Text: "LINE3", Mode: InsertReplacement},
		{Kind: EditDelete, Anchor: 3},
	}
	got, ok := Recover(base, live, edits)
	require.True(t, ok)
	require.Equal(t, "LINE1\nline2\nLINE3\nline4\n", got)
}

func TestRecoverConflictingDriftFails(t *testing.T) {
	t.Parallel()
	base := "line1\nline2\nline3\n"
	// Drift changed the SAME line our edit targets -> conflict -> no recovery.
	live := "line1\nDISK-EDIT\nline3\n"
	edits := []Edit{
		{Kind: EditInsert, Cursor: Cursor{Kind: CursorBeforeAnchor, Line: 2}, Text: "OUR-EDIT", Mode: InsertReplacement},
		{Kind: EditDelete, Anchor: 2},
	}
	_, ok := Recover(base, live, edits)
	require.False(t, ok)
}
