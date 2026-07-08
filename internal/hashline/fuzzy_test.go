package hashline

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// swapEdits builds the insert+delete pair a single-line SWAP lowers to,
// replacing the line at anchor with replacement.
func swapEdits(anchor int, replacement string) []Edit {
	return []Edit{
		{
			Kind:   EditInsert,
			Cursor: Cursor{Kind: CursorBeforeAnchor, Line: anchor},
			Text:   replacement,
			Mode:   InsertReplacement,
		},
		{Kind: EditDelete, Anchor: anchor},
	}
}

func TestRecoverFuzzy_TrailingWhitespaceDriftRelocates(t *testing.T) {
	t.Parallel()

	// base anchored the edit at line 2 ("beta"). live prepended a header line
	// (shifting everything down one) and left trailing whitespace on the
	// anchor line, so the exact tag no longer matches.
	base := "alpha\nbeta\ngamma\n"
	live := "// header\nalpha\nbeta  \ngamma\n"

	got, ok := RecoverFuzzy(base, live, swapEdits(2, "BETA"), DefaultFuzzyThreshold)
	require.True(t, ok)
	require.Equal(t, "// header\nalpha\nBETA\ngamma\n", got)
}

func TestRecoverFuzzy_AmbiguousAnchorBails(t *testing.T) {
	t.Parallel()

	// The anchor content ("target") now appears twice in live. Fuzzy recovery
	// must never guess which occurrence the edit meant.
	base := "alpha\ntarget\ngamma\n"
	live := "target\nalpha\ntarget\ngamma\n"

	_, ok := RecoverFuzzy(base, live, swapEdits(2, "CHANGED"), DefaultFuzzyThreshold)
	require.False(t, ok)
}

func TestRecoverFuzzy_MissingAnchorBails(t *testing.T) {
	t.Parallel()

	// The anchor content is gone from live entirely; there is nothing to
	// relocate onto.
	base := "alpha\ntarget\ngamma\n"
	live := "alpha\ncompletely_different_line\ngamma\n"

	_, ok := RecoverFuzzy(base, live, swapEdits(2, "CHANGED"), DefaultFuzzyThreshold)
	require.False(t, ok)
}

func TestRecoverFuzzy_NoAnchorsBails(t *testing.T) {
	t.Parallel()

	// A BOF-only insert carries no content anchor to verify, so fuzzy recovery
	// refuses rather than relocate blindly.
	base := "alpha\nbeta\n"
	live := "// header\nalpha\nbeta\n"
	edits := []Edit{{Kind: EditInsert, Cursor: Cursor{Kind: CursorBOF}, Text: "top"}}

	_, ok := RecoverFuzzy(base, live, edits, DefaultFuzzyThreshold)
	require.False(t, ok)
}

func TestNormalizeForFuzzy(t *testing.T) {
	t.Parallel()

	require.Equal(t, "beta", normalizeForFuzzy("  beta  "))
	require.Equal(t, "foo bar", normalizeForFuzzy("foo\t \t bar"))
	require.Equal(t, "", normalizeForFuzzy("   "))
	require.Equal(t, `say "hi"`, normalizeForFuzzy("say \u201Chi\u201D"))
	require.Equal(t, "a-b", normalizeForFuzzy("a\u2014b"))
}

func TestFuzzySimilarityPercent(t *testing.T) {
	t.Parallel()

	require.Equal(t, 100, fuzzySimilarityPercent("abc", "abc"))
	require.Equal(t, 0, fuzzySimilarityPercent("abc", "xyz"))
	require.Equal(t, 75, fuzzySimilarityPercent("abcd", "abce"))
}
