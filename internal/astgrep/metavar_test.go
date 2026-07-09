package astgrep

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExtractMetaVar exhaustively exercises the metavariable tokenizer for both
// the default '$' sigil and the 'µ' expando sigil used by grammars where '$' is
// not a valid identifier character (Go, Python, …). Tokens are built from the
// active sigil so the same table drives both.
func TestExtractMetaVar(t *testing.T) {
	t.Parallel()

	type tc struct {
		name      string
		tok       func(s string) string
		wantOK    bool
		wantKind  metaKind
		wantName  string
		wantNamed bool
	}
	cases := []tc{
		{"capture", func(s string) string { return s + "A" }, true, metaCapture, "A", true},
		{"capture_alnum_underscore", func(s string) string { return s + "A1_B" }, true, metaCapture, "A1_B", true},
		{"dropped_bare", func(s string) string { return s + "_" }, true, metaDropped, "", true},
		{"dropped_named", func(s string) string { return s + "_NAME" }, true, metaDropped, "", true},
		{"multiple", func(s string) string { return s + s + s }, true, metaMultiple, "", false},
		{"multi_capture", func(s string) string { return s + s + s + "ARGS" }, true, metaMultiCapture, "ARGS", false},
		{"multi_dropped", func(s string) string { return s + s + s + "_X" }, true, metaMultiple, "", false},
		{"unnamed_capture", func(s string) string { return s + s + "VAR" }, true, metaCapture, "VAR", false},
		// Not metavariables:
		{"lowercase_not_meta", func(s string) string { return s + "foo" }, false, 0, "", false},
		{"digit_first_not_meta", func(s string) string { return s + "1" }, false, 0, "", false},
		{"bare_sigil_not_meta", func(s string) string { return s }, false, 0, "", false},
		{"double_bare_not_meta", func(s string) string { return s + s }, false, 0, "", false},
	}

	for _, sigil := range []rune{'$', 'µ'} {
		sigil := sigil
		for _, c := range cases {
			c := c
			t.Run(fmt.Sprintf("%c/%s", sigil, c.name), func(t *testing.T) {
				t.Parallel()
				mv, ok := extractMetaVar(c.tok(string(sigil)), sigil)
				require.Equal(t, c.wantOK, ok, "ok mismatch for %q", c.tok(string(sigil)))
				if !c.wantOK {
					return
				}
				require.Equal(t, c.wantKind, mv.kind, "kind")
				require.Equal(t, c.wantName, mv.name, "name")
				require.Equal(t, c.wantNamed, mv.named, "named")
			})
		}
	}
}

// TestExtractMetaVarSigilIsolation verifies the tokenizer only honors the sigil
// it is given: a '$' token is inert under the 'µ' sigil and vice-versa.
func TestExtractMetaVarSigilIsolation(t *testing.T) {
	t.Parallel()

	_, ok := extractMetaVar("$A", 'µ')
	require.False(t, ok, "$A must not be a metavar under the µ sigil")

	_, ok = extractMetaVar("µA", '$')
	require.False(t, ok, "µA must not be a metavar under the $ sigil")

	mv, ok := extractMetaVar("µA", 'µ')
	require.True(t, ok)
	require.Equal(t, metaCapture, mv.kind)
	require.Equal(t, "A", mv.name)

	mvMulti, ok := extractMetaVar("µµµARGS", 'µ')
	require.True(t, ok)
	require.Equal(t, metaMultiCapture, mvMulti.kind)
	require.Equal(t, "ARGS", mvMulti.name)
}

// TestMetaCharClassifiers exercises the small character-class helpers directly.
func TestMetaCharClassifiers(t *testing.T) {
	t.Parallel()

	require.True(t, validFirstMetaChar('A'))
	require.True(t, validFirstMetaChar('Z'))
	require.True(t, validFirstMetaChar('_'))
	require.False(t, validFirstMetaChar('a'))
	require.False(t, validFirstMetaChar('0'))

	require.True(t, validMetaChar('A'))
	require.True(t, validMetaChar('_'))
	require.True(t, validMetaChar('9'))
	require.False(t, validMetaChar('a'))
	require.False(t, validMetaChar('-'))

	require.True(t, allMetaChars("A1_B"))
	require.True(t, allMetaChars(""))
	require.False(t, allMetaChars("Ab"))
}
