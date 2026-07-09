package astgrep

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRewrite is the core rewrite table: metavariable substitution, $$$
// preservation, deletion, unbound-metavariable expansion, literal '$'
// preservation and correct replacement counts.
func TestRewrite(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, path, src, pattern, repl string
		wantOut                        string
		wantN                          int
	}{
		{
			name: "or_to_nullish_single", path: "a.ts",
			src: "const x = a || b;\n", pattern: "$A || $B", repl: "$A ?? $B",
			wantOut: "const x = a ?? b;\n", wantN: 1,
		},
		{
			name: "or_to_nullish_multiple", path: "a.ts",
			src: "const x = a || b;\nconst y = c || d;\n", pattern: "$A || $B", repl: "$A ?? $B",
			wantOut: "const x = a ?? b;\nconst y = c ?? d;\n", wantN: 2,
		},
		{
			name: "ellipsis_preserves_spacing", path: "a.ts",
			src: "oldApi(a,  b,   c);\n", pattern: "oldApi($$$A)", repl: "newApi($$$A)",
			wantOut: "newApi(a,  b,   c);\n", wantN: 1,
		},
		{
			name: "deletion_empty_replacement", path: "a.ts",
			src: "foo(a);\nbar();\n", pattern: "foo($X);", repl: "",
			wantOut: "\nbar();\n", wantN: 1,
		},
		{
			name: "unbound_metavar_expands_empty", path: "a.ts",
			src: "foo(a)", pattern: "foo($X)", repl: "baz($Y)",
			wantOut: "baz()", wantN: 1,
		},
		{
			name: "literal_dollar_preserved", path: "a.ts",
			src: "foo(a)", pattern: "foo($X)", repl: "cost$ = $X",
			wantOut: "cost$ = a", wantN: 1,
		},
		{
			name: "go_expando_rewrite", path: "a.go",
			// Replacement uses the literal '$' sigil even though Go compiles with µ.
			src: "package m\nfunc f() int { return a + b }\n", pattern: "$A + $B", repl: "$A - $B",
			wantOut: "package m\nfunc f() int { return a - b }\n", wantN: 1,
		},
		{
			name: "no_match_unchanged", path: "a.ts",
			src: "const x = a && b;\n", pattern: "$A || $B", repl: "$A ?? $B",
			wantOut: "const x = a && b;\n", wantN: 0,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, n, err := Rewrite(c.pattern, c.repl, c.src, c.path)
			require.NoError(t, err)
			require.Equal(t, c.wantN, n, "count")
			require.Equal(t, c.wantOut, out)
		})
	}
}

// TestRewriteNonOverlapping verifies that when matches nest/overlap, only the
// outermost non-overlapping match is applied so text is never double-rewritten.
func TestRewriteNonOverlapping(t *testing.T) {
	t.Parallel()

	// a + b + c parses as (a + b) + c; both the outer and inner additions match
	// $A + $B, but only the outer (document-order-first) one is applied.
	out, n, err := Rewrite("$A + $B", "add($A, $B)", "package m\nfunc f() int { return a + b + c }\n", "a.go")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, "package m\nfunc f() int { return add(a + b, c) }\n", out)
}

// TestRewriteStress checks the count for many replacements at once.
func TestRewriteStress(t *testing.T) {
	t.Parallel()

	const n = 500
	var in, want strings.Builder
	for i := 0; i < n; i++ {
		in.WriteString("x = a || b;\n")
		want.WriteString("x = a ?? b;\n")
	}
	out, count, err := Rewrite("$A || $B", "$A ?? $B", in.String(), "a.ts")
	require.NoError(t, err)
	require.Equal(t, n, count)
	require.Equal(t, want.String(), out)
}

// TestRewriteViaCompiledPattern exercises the (*Pattern).Rewrite entry point.
func TestRewriteViaCompiledPattern(t *testing.T) {
	t.Parallel()

	p, err := Compile("$A || $B", "a.ts")
	require.NoError(t, err)

	out, n, err := p.Rewrite("const x = a || b;\n", "$A ?? $B")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, "const x = a ?? b;\n", out)
}

// TestRewriteUnknownExtensionError ensures Rewrite reports the language error.
func TestRewriteUnknownExtensionError(t *testing.T) {
	t.Parallel()

	_, _, err := Rewrite("$X", "$X", "src", "a.zzz_unknown_ext")
	require.Error(t, err)
}

// TestSubstitute unit-tests the replacement-template expander directly.
func TestSubstitute(t *testing.T) {
	t.Parallel()

	bind := map[string]string{"A": "x", "B": "y", "ARGS": "p, q"}

	require.Equal(t, "x ?? y", substitute("$A ?? $B", bind))
	require.Equal(t, "call(p, q)", substitute("call($$$ARGS)", bind))
	// Unbound metavariable expands to empty.
	require.Equal(t, "()", substitute("($MISSING)", bind))
	// A '$' not forming a metavariable is emitted literally.
	require.Equal(t, "cost$ = x", substitute("cost$ = $A", bind))
	require.Equal(t, "$", substitute("$", bind))
	require.Equal(t, "a $ b", substitute("a $ b", bind))
	// Lowercase after '$' is not a metavariable, so '$' stays literal.
	require.Equal(t, "$foo", substitute("$foo", bind))
}
