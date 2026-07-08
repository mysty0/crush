package hashline

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRepairTrailingKeeperDropped covers the common off-by-one keeper: a SWAP
// range extends one line too far and the payload restates the surviving line
// just past the range. The trailing echo is dropped and the file is not left
// with a duplicated boundary line.
func TestRepairTrailingKeeperDropped(t *testing.T) {
	t.Parallel()
	text := "one\ntwo\nthree\nfour\n"
	// Intent: replace lines 1..3 (one/two/three) with one/X/three, leaving
	// "four" untouched. The model restated the surviving line 4 in the payload.
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 1.=3:\n+one\n+X\n+three\n+four")
	require.Equal(t, "one\nX\nthree\nfour\n", got)
	require.Contains(t, strings.Join(warns, "\n"), "off-by-one keeper")
}

// TestRepairIntendedDuplicateKept ensures a legitimately intended duplicate of a
// bordering line is NOT dropped: a single-line SWAP whose payload deliberately
// repeats a following statement stays intact (no false positive).
func TestRepairIntendedDuplicateKept(t *testing.T) {
	t.Parallel()
	text := "push(a)\npush(b)\npush(c)\n"
	// Replace line 2 with two pushes, intentionally duplicating the existing
	// push(c) on line 3. This must apply verbatim.
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 2.=2:\n+push(b)\n+push(c)")
	require.Equal(t, "push(a)\npush(b)\npush(c)\npush(c)\n", got)
	require.Empty(t, warns)
}

// TestRepairNoEchoUnchanged ensures a payload that does not restate any
// bordering line is applied exactly as written, with no warning.
func TestRepairNoEchoUnchanged(t *testing.T) {
	t.Parallel()
	text := "one\ntwo\nthree\nfour\n"
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 2.=3:\n+alpha\n+beta")
	require.Equal(t, "one\nalpha\nbeta\nfour\n", got)
	require.Empty(t, warns)
}

// TestRepairLeadingKeeperDropped covers the mirror case: the payload restates
// the surviving line just above the range. The leading echo is dropped.
func TestRepairLeadingKeeperDropped(t *testing.T) {
	t.Parallel()
	text := "alpha\nbeta\ngamma\ndelta\n"
	// Intent: replace lines 2..3 (beta/gamma) with beta2, leaving line 1
	// ("alpha") untouched. The model restated the surviving line 1 as the
	// payload head.
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 2.=3:\n+alpha\n+beta2")
	require.Equal(t, "alpha\nbeta2\ndelta\n", got)
	require.Contains(t, strings.Join(warns, "\n"), "off-by-one keeper")
}

// TestRepairTwoSidedEchoDropped covers a balance-neutral echo on both edges:
// the payload restates the surviving lines above and below the range.
func TestRepairTwoSidedEchoDropped(t *testing.T) {
	t.Parallel()
	text := "a\nb\nc\nd\ne\n"
	// Intent: replace lines 2..4 (b/c/d) with "X", but the model restated the
	// surviving neighbors "a" (line 1) and "e" (line 5) on both payload edges.
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 2.=4:\n+a\n+X\n+e")
	require.Equal(t, "a\nX\ne\n", got)
	require.Contains(t, strings.Join(warns, "\n"), "boundary echo")
}

// TestRepairSingleLineNonCloserNotDropped guards the conservative single-line
// rule: a one-line SWAP whose trailing echo is ordinary content (not a
// structural closer) is left alone even though it restates the following line.
func TestRepairSingleLineNonCloserNotDropped(t *testing.T) {
	t.Parallel()
	text := "x := 1\ny := 2\nz := 3\n"
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 2.=2:\n+y := 2\n+z := 3")
	require.Equal(t, "x := 1\ny := 2\nz := 3\nz := 3\n", got)
	require.Empty(t, warns)
}

// TestRepairSingleLineRangeNeverRepaired documents the conservative rule that a
// one-sided echo on a single-line SWAP is never auto-repaired, since an ordinary
// duplicated statement bordering it may be intentional.
func TestRepairSingleLineRangeNeverRepaired(t *testing.T) {
	t.Parallel()
	text := "func f() {\n\treturn 1\n}\n"
	// Restates the surviving closing brace on line 3, but a single-line range is
	// left untouched.
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 2.=2:\n+\treturn 0\n+\treturn 0")
	require.Equal(t, "func f() {\n\treturn 0\n\treturn 0\n}\n", got)
	require.Empty(t, warns)
}

// TestRepairImbalancedTrailingEchoKept ensures the balance-neutrality guard
// holds: when dropping the trailing echo would disturb delimiter balance, the
// payload is applied verbatim rather than corrupted.
func TestRepairImbalancedTrailingEchoKept(t *testing.T) {
	t.Parallel()
	text := "if cond {\n\told := 1\n}\nmore()\n"
	// The payload restates the surviving "}" on line 3, but dropping it would
	// leave the group's opening brace unbalanced, so it must be applied verbatim.
	got, warns := applyPatch(t, text, "[f#0000]\nSWAP 1.=2:\n+if cond {\n+\tnew := 2\n+}")
	require.Equal(t, "if cond {\n\tnew := 2\n}\n}\nmore()\n", got)
	require.Empty(t, warns)
}
