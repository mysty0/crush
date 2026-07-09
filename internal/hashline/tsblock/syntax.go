package tsblock

import (
	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// HasSyntaxError reports whether text has a tree-sitter syntax error for the
// language inferred from path. The second return is false when the language is
// unknown or cannot be parsed, in which case callers should not treat the file
// as validated (there is no signal either way).
func HasSyntaxError(text, path string) (bad, ok bool) {
	entry := grammars.DetectLanguage(path)
	if entry == nil {
		return false, false
	}
	lang := entry.Language()
	if lang == nil {
		return false, false
	}
	parser := gts.NewParser(lang)
	tree, err := parser.Parse([]byte(text))
	if err != nil || tree == nil {
		return false, false
	}
	root := tree.RootNode()
	if root == nil {
		return false, false
	}
	return root.HasError(), true
}

// IntroducesSyntaxError reports whether editing oldText into newText turns a
// cleanly-parsing file into one with a syntax error. It returns false unless
// the language is known, oldText parses clean, and newText does not — so a file
// that was already broken (or whose language is unknown) never trips the gate.
func IntroducesSyntaxError(oldText, newText, path string) bool {
	oldBad, oldOK := HasSyntaxError(oldText, path)
	if !oldOK || oldBad {
		return false
	}
	newBad, newOK := HasSyntaxError(newText, path)
	return newOK && newBad
}
