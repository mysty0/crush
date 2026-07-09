package astgrep

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCompileErrors covers the compile-time failure modes: unknown language,
// empty pattern and a root that is a bare ellipsis metavariable.
func TestCompileErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, pattern, path string
		wantErrSubstr       string
	}{
		{"unknown_extension", "$X", "a.zzz_unknown_ext", "cannot infer language"},
		{"empty_pattern", "", "a.ts", "empty pattern"},
		{"whitespace_pattern", "   \n\t ", "a.ts", "empty pattern"},
		{"bare_ellipsis_ts", "$$$", "a.ts", "root cannot be an ellipsis"},
		{"bare_ellipsis_go", "$$$", "a.go", "root cannot be an ellipsis"},
		{"bare_ellipsis_capture_ts", "$$$ARGS", "a.ts", "root cannot be an ellipsis"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, err := Compile(c.pattern, c.path)
			require.Error(t, err)
			require.Nil(t, p)
			require.Contains(t, err.Error(), c.wantErrSubstr)
		})
	}
}

// TestCompileSucceedsForValidPatterns is a sanity check that ordinary patterns
// compile across languages, including the expando path.
func TestCompileSucceedsForValidPatterns(t *testing.T) {
	t.Parallel()

	cases := []struct{ pattern, path string }{
		{"$A || $B", "a.ts"},
		{"foo($$$ARGS)", "a.ts"},
		{"$A + $B", "a.go"},
		{"$X > 0", "a.py"},
		{"$X.foo()", "a.js"},
		{"$X.foo()", "a.tsx"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.pattern+"@"+c.path, func(t *testing.T) {
			t.Parallel()
			p, err := Compile(c.pattern, c.path)
			require.NoError(t, err)
			require.NotNil(t, p)
		})
	}
}

// TestSearchDoesNotPanicOnEmptySource guards the trivial-input paths.
func TestSearchDoesNotPanicOnEmptySource(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		ms, err := Search("return $X", "", "a.ts")
		require.NoError(t, err)
		require.Empty(t, ms)
	})
}
