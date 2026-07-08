package tsblock

import (
	"testing"

	"github.com/charmbracelet/crush/internal/hashline"
	"github.com/stretchr/testify/require"
)

func resolve(t *testing.T, path, text string, line int) (hashline.BlockSpan, bool) {
	t.Helper()
	return New().ResolveBlock(hashline.BlockRequest{Path: path, Text: text, Line: line})
}

func TestResolveGoFunction(t *testing.T) {
	t.Parallel()
	src := "package main\n\nfunc hello() {\n\tprintln(\"hi\")\n}\n\nfunc world() {\n\treturn\n}\n"
	span, ok := resolve(t, "main.go", src, 3)
	require.True(t, ok)
	require.Equal(t, 3, span.Start)
	require.Equal(t, 5, span.End)

	span, ok = resolve(t, "main.go", src, 7)
	require.True(t, ok)
	require.Equal(t, 7, span.Start)
	require.Equal(t, 9, span.End)
}

func TestResolveGoStructAndMethod(t *testing.T) {
	t.Parallel()
	src := "package main\n\ntype T struct {\n\tX int\n}\n\nfunc (t T) M() {\n\treturn\n}\n"
	span, ok := resolve(t, "t.go", src, 3)
	require.True(t, ok)
	require.Equal(t, 3, span.Start)
	require.Equal(t, 5, span.End)

	span, ok = resolve(t, "t.go", src, 7)
	require.True(t, ok)
	require.Equal(t, 7, span.Start)
	require.Equal(t, 9, span.End)
}

func TestResolvePythonDecorator(t *testing.T) {
	t.Parallel()
	src := "@cache\ndef load(key):\n    return store[key]\n"
	// Anchoring on the decorator sweeps the whole decorated definition.
	span, ok := resolve(t, "s.py", src, 1)
	require.True(t, ok)
	require.Equal(t, 1, span.Start)
	require.Equal(t, 3, span.End)

	// Anchoring on the def line resolves just the function.
	span, ok = resolve(t, "s.py", src, 2)
	require.True(t, ok)
	require.Equal(t, 2, span.Start)
	require.Equal(t, 3, span.End)
}

func TestResolveMarkdownSection(t *testing.T) {
	t.Parallel()
	src := "# Title\n\nintro\n\n## Section A\n\nbody a\n\n## Section B\n\nbody b\n"
	span, ok := resolve(t, "doc.md", src, 5)
	require.True(t, ok)
	require.Equal(t, 5, span.Start)
	require.Equal(t, 9, span.End) // heading through its body, up to next same-level heading
}

func TestResolveTypeScriptFunction(t *testing.T) {
	t.Parallel()
	src := "const x = 1;\n\nfunction add(a: number, b: number): number {\n\treturn a + b;\n}\n"
	span, ok := resolve(t, "a.ts", src, 3)
	require.True(t, ok)
	require.Equal(t, 3, span.Start)
	require.Equal(t, 5, span.End)
}

func TestResolveClosingLineIsNotABlock(t *testing.T) {
	t.Parallel()
	src := "package main\n\nfunc hello() {\n\tprintln(\"hi\")\n}\n"
	// Line 5 is the closing brace: no node begins there.
	_, ok := resolve(t, "main.go", src, 5)
	require.False(t, ok)
}

func TestResolveSingleLineStatement(t *testing.T) {
	t.Parallel()
	src := "package main\n\nvar x = 1\n"
	// A single-line declaration resolves to a one-line span; the caller treats
	// that as a mis-anchor.
	span, ok := resolve(t, "main.go", src, 3)
	require.True(t, ok)
	require.Equal(t, span.Start, span.End)
}

func TestResolveUnknownLanguage(t *testing.T) {
	t.Parallel()
	_, ok := resolve(t, "data.unknownext", "some text\nmore\n", 1)
	require.False(t, ok)
}

func TestResolveOutOfRangeLine(t *testing.T) {
	t.Parallel()
	src := "package main\n"
	_, ok := resolve(t, "main.go", src, 99)
	require.False(t, ok)
}

// End-to-end through hashline.ResolveBlockEdits + Apply with the real resolver.
func TestResolveBlockEditsWithRealResolver(t *testing.T) {
	t.Parallel()
	src := "package main\n\nfunc hello() {\n\tprintln(\"hi\")\n}\n"
	sections, _, err := hashline.Parse("[main.go#0000]\nSWAP.BLK 3:\n+func hello() {\n+\tprintln(\"hello, world\")\n+}")
	require.NoError(t, err)

	edits, _, err := hashline.ResolveBlockEdits(sections[0].Edits, src, "main.go", New())
	require.NoError(t, err)
	res, err := hashline.Apply(src, edits)
	require.NoError(t, err)
	require.Equal(t, "package main\n\nfunc hello() {\n\tprintln(\"hello, world\")\n}\n", res.Text)
}

func TestResolveBlockDeleteWithRealResolver(t *testing.T) {
	t.Parallel()
	src := "package main\n\nfunc a() {}\n\nfunc b() {\n\treturn\n}\n"
	sections, _, err := hashline.Parse("[main.go#0000]\nDEL.BLK 5")
	require.NoError(t, err)
	edits, _, err := hashline.ResolveBlockEdits(sections[0].Edits, src, "main.go", New())
	require.NoError(t, err)
	res, err := hashline.Apply(src, edits)
	require.NoError(t, err)
	require.Equal(t, "package main\n\nfunc a() {}\n\n", res.Text)
}

func TestResolveBlockInsertAfterWithRealResolver(t *testing.T) {
	t.Parallel()
	src := "package main\n\nfunc a() {\n\treturn\n}\n"
	sections, _, err := hashline.Parse("[main.go#0000]\nINS.BLK.POST 3:\n+\n+func b() {}")
	require.NoError(t, err)
	edits, warns, err := hashline.ResolveBlockEdits(sections[0].Edits, src, "main.go", New())
	require.NoError(t, err)
	require.Empty(t, warns, "a resolvable block should not lower/ warn")
	res, err := hashline.Apply(src, edits)
	require.NoError(t, err)
	require.Equal(t, "package main\n\nfunc a() {\n\treturn\n}\n\nfunc b() {}\n", res.Text)
}

func TestResolveBlockSingleLineRejected(t *testing.T) {
	t.Parallel()
	src := "package main\n\nvar x = 1\n"
	sections, _, err := hashline.Parse("[main.go#0000]\nSWAP.BLK 3:\n+var x = 2")
	require.NoError(t, err)
	_, _, err = hashline.ResolveBlockEdits(sections[0].Edits, src, "main.go", New())
	require.Error(t, err)
	require.Contains(t, err.Error(), "single line")
}
