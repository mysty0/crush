package astgrep

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolveLanguage checks extension-based language inference and the expando
// char chosen for each family: the JS/TS family keeps '$', while grammars that
// reject '$' in identifiers (Go, Python) use the 'µ' expando.
func TestResolveLanguage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path        string
		wantExpando rune
	}{
		{"a.ts", '$'},
		{"a.tsx", '$'},
		{"a.js", '$'},
		{"a.go", 'µ'},
		{"a.py", 'µ'},
	}
	for _, c := range cases {
		c := c
		t.Run(c.path, func(t *testing.T) {
			t.Parallel()
			lg, ok := resolveLanguage(c.path)
			require.True(t, ok, "expected %q to resolve", c.path)
			require.NotNil(t, lg.lang)
			require.NotEmpty(t, lg.name)
			require.Equal(t, c.wantExpando, lg.expando)
		})
	}
}

// TestResolveLanguageUnknown ensures unknown / missing extensions fail cleanly.
func TestResolveLanguageUnknown(t *testing.T) {
	t.Parallel()

	_, ok := resolveLanguage("a.zzz_unknown_ext")
	require.False(t, ok)

	_, ok = resolveLanguage("noextension")
	require.False(t, ok)

	_, ok = resolveLanguage("")
	require.False(t, ok)
}

// TestPreprocess verifies the '$' -> expando substitution that lets patterns
// parse under grammars where '$' is not identifier-legal. It must rewrite
// metavariable sigils and '$$$' ellipses but leave a lone '$' before a
// non-metavariable char untouched (e.g. JS template `${...}`).
func TestPreprocess(t *testing.T) {
	t.Parallel()

	goLang, ok := resolveLanguage("a.go")
	require.True(t, ok)
	tsLang, ok := resolveLanguage("a.ts")
	require.True(t, ok)

	// The '$' family leaves the pattern verbatim.
	require.Equal(t, "$A + $B", tsLang.preprocess("$A + $B"))
	require.Equal(t, "$$$ARGS", tsLang.preprocess("$$$ARGS"))

	// The 'µ' expando rewrites metavariable and ellipsis sigils.
	require.Equal(t, "µA + µB", goLang.preprocess("$A + $B"))
	require.Equal(t, "µµµ", goLang.preprocess("$$$"))
	require.Equal(t, "µµµARGS", goLang.preprocess("$$$ARGS"))
	require.Equal(t, "µ_", goLang.preprocess("$_"))

	// A lone '$' before a non-metavariable char is preserved.
	require.Equal(t, "${x}", goLang.preprocess("${x}"))
	require.Equal(t, "a $ b", goLang.preprocess("a $ b"))
}
