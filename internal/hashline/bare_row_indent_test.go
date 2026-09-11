package hashline

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBodyRowsPreserveIndentation covers the whitespace contract for body
// rows in both spellings.
//
// A body row's leading whitespace is content, not framing. The parser used
// to hand appendBody a fully space-trimmed line, which was harmless for
// "+text" rows (the sigil is the first character, so nothing is stripped)
// but silently destroyed the indentation of bare rows -- the tolerated
// spelling where the model omits the "+". The result was syntactically
// mangled output: every model-written line flattened to column zero while
// untouched lines kept their indent, seen in practice on a Rust file whose
// whole rewritten region came back unindented.
func TestBodyRowsPreserveIndentation(t *testing.T) {
	t.Parallel()

	const want0 = "    indented(),"
	const want1 = "        deeper(),"

	cases := []struct {
		name string
		in   string
	}{
		{
			name: "sigil rows",
			in:   "[f.go#AAAA]\nSWAP 1.=1:\n+" + want0 + "\n+" + want1 + "\n",
		},
		{
			// The bare spelling: no "+" at all. Previously lost all indent.
			name: "bare rows",
			in:   "[f.go#AAAA]\nSWAP 1.=1:\n" + want0 + "\n" + want1 + "\n",
		},
		{
			// A sigil preceded by stray whitespace must still be found,
			// and must not leak that whitespace into the payload.
			name: "sigil with leading whitespace",
			in:   "[f.go#AAAA]\nSWAP 1.=1:\n  +" + want0 + "\n\t+" + want1 + "\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			secs, _, err := Parse(tc.in)
			require.NoError(t, err)
			require.Len(t, secs, 1)
			require.GreaterOrEqual(t, len(secs[0].Edits), 2)

			require.Equal(t, want0, secs[0].Edits[0].Text,
				"leading whitespace is content and must survive verbatim")
			require.Equal(t, want1, secs[0].Edits[1].Text,
				"deeper indentation must survive verbatim")
		})
	}
}

// TestBareBodyRowStillWarns confirms the tolerance path keeps telling the
// caller it guessed, so preserving indentation does not silently make the
// malformed spelling look correct.
func TestBareBodyRowStillWarns(t *testing.T) {
	t.Parallel()

	_, warns, err := Parse("[f.go#AAAA]\nSWAP 1.=1:\n    indented(),\n")
	require.NoError(t, err)
	require.NotEmpty(t, warns, "a bare body row must still raise the auto-prefix warning")
}
