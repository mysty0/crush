package astgrep

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// language bundles a resolved grammar with its metavariable expando char.
type language struct {
	name    string
	lang    *gts.Language
	expando rune
}

// expandoOverrides maps a language name to the identifier-safe rune used in
// place of '$' when a pattern is parsed, for grammars where '$' is not a valid
// identifier character. Mirrors ast-grep's per-language expando_char. Languages
// absent here keep '$' (valid in the JS family, Java, PHP, etc.).
var expandoOverrides = map[string]rune{
	"go":      'µ',
	"python":  'µ',
	"rust":    'µ',
	"ruby":    'µ',
	"swift":   'µ',
	"csharp":  'µ',
	"c-sharp": 'µ',
	"kotlin":  'µ',
	"scala":   'µ',
	"elixir":  'µ',
	"haskell": 'µ',
	"hcl":     'µ',
	"lua":     'µ',
	"julia":   'µ',
	"c":       'µ',
	"cpp":     'µ',
	"c++":     'µ',
	"css":     '_',
	"scss":    '_',
	"nix":     '_',
}

// resolveLanguage infers the grammar from path (extension) and returns it with
// the expando char to use for metavariables. ok is false for unknown
// extensions.
func resolveLanguage(path string) (language, bool) {
	entry := grammars.DetectLanguage(path)
	if entry == nil {
		return language{}, false
	}
	l := entry.Language()
	if l == nil {
		return language{}, false
	}
	name := strings.ToLower(entry.Name)
	expando := '$'
	if e, ok := expandoOverrides[name]; ok {
		expando = e
	}
	return language{name: name, lang: l, expando: expando}, true
}

// parse builds a syntax tree for src under lang.
func (lg language) parse(src string) (*gts.Tree, error) {
	parser := gts.NewParser(lg.lang)
	return parser.Parse([]byte(src))
}

// preprocess substitutes the language's expando char for '$' ahead of a metavar
// name (or a '$$$' ellipsis) so the pattern parses under grammars that reject
// '$' in identifiers. A lone '$' before a non-metavar char (e.g. JS template
// `${...}`) is left untouched. Mirrors ast-grep's pre_process_pattern.
func (lg language) preprocess(pattern string) string {
	if lg.expando == '$' {
		return pattern
	}
	rs := []rune(pattern)
	var b strings.Builder
	for i := 0; i < len(rs); {
		if rs[i] != '$' {
			b.WriteRune(rs[i])
			i++
			continue
		}
		// Count a run of consecutive '$'.
		j := i
		for j < len(rs) && rs[j] == '$' {
			j++
		}
		dollars := j - i
		var next rune
		if j < len(rs) {
			next = rs[j]
		}
		needReplace := validFirstMetaChar(next) || dollars == 3
		sigil := '$'
		if needReplace {
			sigil = lg.expando
		}
		for k := 0; k < dollars; k++ {
			b.WriteRune(sigil)
		}
		i = j
	}
	return b.String()
}
