// Package astgrep is a pure-Go structural code search and rewrite engine
// modeled on ast-grep. Patterns are written as code fragments with
// metavariable holes ($VAR, $$$VARS, $_); matching compares tree-sitter
// syntax-tree shape rather than text, so it ignores formatting, comments, and
// string contents.
//
// The engine links only the CGO-free gotreesitter runtime (via the grammars
// registry), preserving the CGO_ENABLED=0 build. It implements ast-grep's
// default "smart" match strictness: named leaf tokens must match by text,
// unnamed (punctuation/keyword) candidate nodes are skipped, and trailing
// candidates are ignored.
package astgrep

import "strings"

// metaKind classifies a metavariable token.
type metaKind int

const (
	metaNone         metaKind = iota
	metaCapture               // $NAME  — captures one node
	metaDropped               // $_NAME — matches one node, no binding
	metaMultiple              // $$$    — matches a run of siblings, no binding
	metaMultiCapture          // $$$NAME — captures a run of siblings
)

// metaVar is a parsed metavariable token.
type metaVar struct {
	kind  metaKind
	name  string
	named bool // false for the $$NAME "unnamed node" capture form
}

// validFirstMetaChar reports whether r may start a metavariable name.
func validFirstMetaChar(r rune) bool {
	return r >= 'A' && r <= 'Z' || r == '_'
}

// validMetaChar reports whether r may appear in a metavariable name.
func validMetaChar(r rune) bool {
	return validFirstMetaChar(r) || r >= '0' && r <= '9'
}

// extractMetaVar parses src (a single leaf token's text) as a metavariable,
// using meta as the sigil (the language's expando char). It mirrors
// ast-grep-core's meta_var.rs: names are uppercase/underscore-led, a leading
// underscore marks a non-capturing hole, a doubled sigil marks an unnamed-node
// capture, and a tripled sigil marks a sibling-sequence (ellipsis) match.
func extractMetaVar(src string, meta rune) (metaVar, bool) {
	m := string(meta)
	triple := m + m + m
	if src == triple {
		return metaVar{kind: metaMultiple}, true
	}
	if rest, ok := strings.CutPrefix(src, triple); ok {
		if !allMetaChars(rest) || rest == "" {
			return metaVar{}, false
		}
		if rest[0] == '_' {
			return metaVar{kind: metaMultiple}, true
		}
		return metaVar{kind: metaMultiCapture, name: rest}, true
	}

	rest, ok := strings.CutPrefix(src, m)
	if !ok {
		return metaVar{}, false
	}
	named := true
	if r, ok := strings.CutPrefix(rest, m); ok {
		named = false
		rest = r
	}
	if rest == "" {
		return metaVar{}, false
	}
	rs := []rune(rest)
	if !validFirstMetaChar(rs[0]) {
		return metaVar{}, false
	}
	for _, r := range rs[1:] {
		if !validMetaChar(r) {
			return metaVar{}, false
		}
	}
	if rs[0] == '_' {
		return metaVar{kind: metaDropped, named: named}, true
	}
	return metaVar{kind: metaCapture, name: rest, named: named}, true
}

// allMetaChars reports whether every rune of s is a valid metavariable char.
func allMetaChars(s string) bool {
	for _, r := range s {
		if !validMetaChar(r) {
			return false
		}
	}
	return true
}
