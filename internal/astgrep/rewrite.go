package astgrep

import (
	"sort"
	"strings"
)

// Rewrite compiles pattern (language inferred from path) and replaces every
// non-overlapping match in source with replacement, substituting $VAR and
// $$$VAR from each match's captures. It returns the new source and the number
// of replacements.
func Rewrite(pattern, replacement, source, path string) (string, int, error) {
	p, err := Compile(pattern, path)
	if err != nil {
		return "", 0, err
	}
	return p.Rewrite(source, replacement)
}

// Rewrite applies the compiled pattern to source.
func (p *Pattern) Rewrite(source, replacement string) (string, int, error) {
	matches, err := p.Search(source)
	if err != nil {
		return "", 0, err
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].StartByte < matches[j].StartByte })

	src := []byte(source)
	var out strings.Builder
	pos, count, lastEnd := 0, 0, 0
	for _, m := range matches {
		if m.StartByte < lastEnd || m.StartByte < pos {
			continue // overlapping with an already-applied replacement
		}
		out.Write(src[pos:m.StartByte])
		out.WriteString(substitute(replacement, m.Bindings))
		pos = m.EndByte
		lastEnd = m.EndByte
		count++
	}
	out.Write(src[pos:])
	return out.String(), count, nil
}

// substitute expands $VAR and $$$VAR in a replacement template from bindings.
// The replacement is written by the user with the literal '$' sigil regardless
// of the language's expando char. Unbound metavariables expand to empty; a '$'
// not forming a metavariable is emitted literally.
func substitute(tmpl string, bindings map[string]string) string {
	rs := []rune(tmpl)
	var out strings.Builder
	for i := 0; i < len(rs); {
		if rs[i] != '$' {
			out.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		for j < len(rs) && rs[j] == '$' {
			j++
		}
		k := j
		for k < len(rs) && validMetaChar(rs[k]) {
			k++
		}
		token := string(rs[i:k])
		if mv, ok := extractMetaVar(token, '$'); ok && (mv.kind == metaCapture || mv.kind == metaMultiCapture) {
			out.WriteString(bindings[mv.name])
			i = k
			continue
		}
		out.WriteString(string(rs[i:j]))
		i = j
	}
	return out.String()
}
