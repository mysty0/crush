package astgrep

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnclosingSymbol verifies the nearest enclosing function/method/class name
// reported for each match across languages.
//
// The "ts_arrow_const" case checks that a match inside an arrow function bound
// to a `const` reports the binding name (the walk falls back from the unnamed
// arrow_function to its variable_declarator).
func TestEnclosingSymbol(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, path, src, pattern string
		wantEnclosing            string
	}{
		{
			name: "go_func", path: "a.go",
			src:     "package m\n\nfunc add(a int, b int) int {\n\treturn a + b\n}\n",
			pattern: "$A + $B", wantEnclosing: "add",
		},
		{
			name: "go_method", path: "a.go",
			src:     "package m\n\nfunc (r R) Method() int {\n\treturn bar()\n}\n",
			pattern: "return $X", wantEnclosing: "Method",
		},
		{
			name: "go_nested_func_literal", path: "a.go",
			// A func literal has no name; the walk climbs to the outer function.
			src:     "package m\n\nfunc Outer() {\n\tinner := func() int {\n\t\treturn bar()\n\t}\n\t_ = inner\n}\n",
			pattern: "return $X", wantEnclosing: "Outer",
		},
		{
			name: "ts_function", path: "a.ts",
			src:     "function f() {\n  return bar();\n}\n",
			pattern: "return $X", wantEnclosing: "f",
		},
		{
			name: "ts_class_method", path: "a.ts",
			src:     "class C {\n  m() {\n    return bar();\n  }\n}\n",
			pattern: "return $X", wantEnclosing: "m",
		},
		{
			name: "ts_nested_function", path: "a.ts",
			src:     "function outer() {\n  function inner() {\n    return bar();\n  }\n}\n",
			pattern: "return $X", wantEnclosing: "inner",
		},
		{
			name: "ts_arrow_const", path: "a.ts",
			src:     "const foo = () => {\n  return bar();\n};\n",
			pattern: "return $X", wantEnclosing: "foo",
		},
		{
			name: "py_def", path: "a.py",
			src:     "def f():\n    return bar()\n",
			pattern: "return $X", wantEnclosing: "f",
		},
		{
			name: "py_method", path: "a.py",
			src:     "class C:\n    def m(self):\n        return bar()\n",
			pattern: "return $X", wantEnclosing: "m",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ms := searchOK(t, c.pattern, c.src, c.path)
			require.Len(t, ms, 1, "%s", dump(ms))
			require.Equal(t, c.wantEnclosing, ms[0].Enclosing)
		})
	}
}

// TestMatchLocations checks that each match reports the correct 1-based start
// line, in document order, for multiple matches in one file.
func TestMatchLocations(t *testing.T) {
	t.Parallel()

	src := "function a() {\n  return one;\n}\nfunction b() {\n  return two;\n}\nfunction c() {\n  return three;\n}\n"
	ms := searchOK(t, "return $X", src, "a.ts")
	require.Len(t, ms, 3)

	gotLines := []int{ms[0].StartLine, ms[1].StartLine, ms[2].StartLine}
	require.Equal(t, []int{2, 5, 8}, gotLines)

	gotBind := []string{ms[0].Bindings["X"], ms[1].Bindings["X"], ms[2].Bindings["X"]}
	require.Equal(t, []string{"one", "two", "three"}, gotBind)

	// StartCol is 1-based; "return" begins after two spaces of indentation.
	require.Equal(t, 3, ms[0].StartCol)

	// Byte offsets are consistent with the reported text.
	for _, m := range ms {
		require.Equal(t, m.Text, src[m.StartByte:m.EndByte])
		require.GreaterOrEqual(t, m.EndLine, m.StartLine)
	}
}

// TestNoMatch confirms a no-match search returns an empty result and no error.
func TestNoMatch(t *testing.T) {
	t.Parallel()

	ms, err := Search("return $X", "const x = 1;\n", "a.ts")
	require.NoError(t, err)
	require.Empty(t, ms)
}

// TestUnicodeSourceAndIdentifiers verifies matching works with non-ASCII
// identifiers and source text and preserves them in captures.
func TestUnicodeSourceAndIdentifiers(t *testing.T) {
	t.Parallel()

	ms := searchOK(t, "$X + $Y", "const r = café + naïve;\n", "a.ts")
	require.Len(t, ms, 1)
	require.Equal(t, "café + naïve", ms[0].Text)
	require.Equal(t, "café", ms[0].Bindings["X"])
	require.Equal(t, "naïve", ms[0].Bindings["Y"])
}

// TestSyntaxErrorResilience ensures a source with a syntax error still yields
// matches from the valid regions and never panics.
func TestSyntaxErrorResilience(t *testing.T) {
	t.Parallel()

	// The second function has a broken parameter list; the first is valid.
	src := "function f() {\n  return ok;\n}\nfunction g( {\n  return also;\n}\n"
	ms, err := Search("return $X", src, "a.ts")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(ms), 1)

	var texts []string
	for _, m := range ms {
		texts = append(texts, m.Text)
	}
	require.Contains(t, texts, "return ok;")
}

// TestStressManyMatches checks the engine finds every occurrence in a large
// input.
func TestStressManyMatches(t *testing.T) {
	t.Parallel()

	const n = 500
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("f(a || b);\n")
	}
	ms := searchOK(t, "$A || $B", b.String(), "a.ts")
	require.Len(t, ms, n)
}

// TestSearchUnknownExtensionError ensures Search surfaces an error rather than
// panicking on an unknown extension.
func TestSearchUnknownExtensionError(t *testing.T) {
	t.Parallel()

	_, err := Search("$X", "whatever", "a.zzz_unknown_ext")
	require.Error(t, err)
}
