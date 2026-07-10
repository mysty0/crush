package astgrep

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// searchOK is a small helper: it runs Search and fails on error.
func searchOK(t *testing.T, pattern, src, path string) []Match {
	t.Helper()
	ms, err := Search(pattern, src, path)
	require.NoError(t, err)
	return ms
}

// TestMatchCounts is the core matching table: exact match counts across
// languages covering single/multi metavariables, consistent bindings, nesting,
// literal sensitivity, kind precision, formatting invariance and the
// per-language expando path.
func TestMatchCounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, path, src, pattern string
		wantN                    int
	}{
		// --- single metavariable ---
		{"ts_member_call", "a.ts", "a.foo();\nb.bar();\nc.foo();\n", "$X.foo()", 2},
		{"ts_return", "a.ts", "function f() { return x; }\nfunction g() { return y; }\n", "return $X", 2},
		{"ts_equality", "a.ts", "const p = a === b;\n", "$A === $B", 1},

		// --- multi metavariable ($$$) ---
		{"ts_call_ellipsis", "a.ts", "foo(a, b, c);\nbar();\nfoo(1);\n", "foo($$$A)", 2},
		{"ts_array_ellipsis", "a.ts", "const a = [1, 2, 3];\n", "[$$$X]", 1},
		{"ts_params_ellipsis", "a.ts", "function g(a, b, c) {}\n", "function $N($$$P) {}", 1},
		{"ts_stmt_seq", "a.ts", "function f() { a(); b(); }\n", "{ $$$BODY }", 1},

		// --- consistent binding ---
		{"ts_consistent_match", "a.ts", "const p = a === a;\n", "$X === $X", 1},
		{"ts_consistent_nomatch", "a.ts", "const p = a === b;\n", "$X === $X", 0},

		// --- nesting ---
		{"ts_nested_if", "a.ts", "function f() { if (ok) { return v; } }\n", "if ($C) { return $X; }", 1},

		// --- literal / text sensitivity ---
		{"ts_literal_match", "a.ts", "foo(1);\nfoo(2);\n", "foo(1)", 1},
		{"ts_literal_nomatch", "a.ts", "foo(2);\n", "foo(1)", 0},
		{"ts_metavar_matches_both", "a.ts", "foo(1);\nfoo(2);\n", "foo($X)", 2},

		// --- kind precision: || must not match ?? or && ---
		{"ts_or_not_nullish", "a.ts", "const x = a ?? b;\n", "$A || $B", 0},
		{"ts_or_not_and", "a.ts", "const x = a && b;\n", "$A || $B", 0},
		{"ts_or_match", "a.ts", "const x = a || b;\n", "$A || $B", 1},

		// --- string / comment contents must not match ---
		{"ts_or_in_string", "a.ts", "const s = \"a || b\";\n", "$A || $B", 0},
		{"ts_or_in_comment", "a.ts", "// a || b\nconst x = 1;\n", "$A || $B", 0},

		// --- formatting invariance ---
		{"ts_fmt_extra_spaces", "a.ts", "foo(a,  b);\n", "foo(a, b)", 1},
		{"ts_fmt_multiline", "a.ts", "foo(a,\n    b);\n", "foo(a, b)", 1},
		{"ts_fmt_pattern_multiline", "a.ts", "foo(a, b);\n", "foo(a,\n  b)", 1},

		// --- cross-language expando path (Go/Python use µ internally) ---
		{"go_add_expando", "a.go", "package m\nfunc f() int { return a + b }\n", "$A + $B", 1},
		{"go_or_expando", "a.go", "package m\nfunc f() bool { return a || b }\n", "$A || $B", 1},
		{"py_add_expando", "a.py", "x = a + b\n", "$A + $B", 1},
		{"py_compare", "a.py", "def f(x):\n    if x > 0:\n        return x\n    return 0\n", "$X > 0", 1},

		// --- other languages in the JS family ---
		{"js_member_call", "a.js", "obj.foo();\n", "$X.foo()", 1},
		{"tsx_member_call", "a.tsx", "const e = <div>{a.foo()}</div>;\n", "$X.foo()", 1},

		// --- no match returns empty ---
		{"ts_no_match", "a.ts", "const x = 1;\n", "return $X", 0},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ms := searchOK(t, c.pattern, c.src, c.path)
			require.Len(t, ms, c.wantN, "matches: %s", dump(ms))
		})
	}
}

// TestMatchBindings checks exact captured bindings and match text for single
// and $$$ multi-captures, including verbatim spacing preservation on $$$.
func TestMatchBindings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, path, src, pattern string
		wantText                 string
		wantBind                 map[string]string
	}{
		{
			name: "member_call", path: "a.ts",
			src: "user.foo();\n", pattern: "$X.foo()",
			wantText: "user.foo()", wantBind: map[string]string{"X": "user"},
		},
		{
			name: "call_ellipsis", path: "a.ts",
			src: "foo(a, b, c);\n", pattern: "foo($$$A)",
			wantText: "foo(a, b, c)", wantBind: map[string]string{"A": "a, b, c"},
		},
		{
			name: "ellipsis_preserves_spacing", path: "a.ts",
			src: "foo(a,   b ,c);\n", pattern: "foo($$$A)",
			wantText: "foo(a,   b ,c)", wantBind: map[string]string{"A": "a,   b ,c"},
		},
		{
			name: "params_ellipsis", path: "a.ts",
			src: "function g(a, b, c) {}\n", pattern: "function $N($$$P) {}",
			wantText: "function g(a, b, c) {}", wantBind: map[string]string{"N": "g", "P": "a, b, c"},
		},
		{
			name: "const_arrow", path: "a.ts",
			src: "const add = (a, b) => a + b;\n", pattern: "const $NAME = ($$$ARGS) => $BODY",
			wantText: "const add = (a, b) => a + b;", wantBind: map[string]string{"NAME": "add", "ARGS": "a, b", "BODY": "a + b"},
		},
		{
			name: "consistent_capture", path: "a.ts",
			src: "const p = a === a;\n", pattern: "$X === $X",
			wantText: "a === a", wantBind: map[string]string{"X": "a"},
		},
		{
			name: "go_binary", path: "a.go",
			src: "package m\nfunc f() int { return a + b }\n", pattern: "$A + $B",
			wantText: "a + b", wantBind: map[string]string{"A": "a", "B": "b"},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ms := searchOK(t, c.pattern, c.src, c.path)
			require.Len(t, ms, 1, "%s", dump(ms))
			require.Equal(t, c.wantText, ms[0].Text)
			require.Equal(t, c.wantBind, ms[0].Bindings)
		})
	}
}

// TestDroppedMetavarNoBinding verifies $_ matches a node but records no binding.
func TestDroppedMetavarNoBinding(t *testing.T) {
	t.Parallel()

	ms := searchOK(t, "foo($_)", "foo(a);\nfoo(b);\n", "a.ts")
	require.Len(t, ms, 2)
	for _, m := range ms {
		require.Empty(t, m.Bindings)
	}
}

// TestCompileAndPatternSearch exercises the compiled-pattern API and its reuse
// across multiple sources.
func TestCompileAndPatternSearch(t *testing.T) {
	t.Parallel()

	p, err := Compile("$A || $B", "a.ts")
	require.NoError(t, err)
	require.NotNil(t, p)

	ms, err := p.Search("const x = a || b;\n")
	require.NoError(t, err)
	require.Len(t, ms, 1)

	ms, err = p.Search("const y = c || d || e;\n")
	require.NoError(t, err)
	// c || d || e parses as (c || d) || e -> two logical_or nodes.
	require.Len(t, ms, 2)

	ms, err = p.Search("const z = a && b;\n")
	require.NoError(t, err)
	require.Empty(t, ms)
}
