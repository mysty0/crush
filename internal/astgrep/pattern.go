package astgrep

import (
	"fmt"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

// patKind classifies a compiled pattern node.
type patKind int

const (
	patMeta     patKind = iota // a metavariable hole
	patTerminal                // a leaf token (identifier, keyword, punctuation)
	patInternal                // an interior node matched by kind + children
)

// patNode is one node of a compiled pattern tree.
type patNode struct {
	kind     patKind
	mv       metaVar   // patMeta
	text     string    // patTerminal: exact token text
	isNamed  bool      // patTerminal: whether the token is a named node
	tsKind   string    // patTerminal/patInternal: tree-sitter node kind
	children []patNode // patInternal
}

// Pattern is a compiled structural pattern ready to match against source.
type Pattern struct {
	root     patNode
	lg       language
	rootKind string // tree-sitter kind of the pattern root, "" when it is a metavar
}

// Compile builds a Pattern from a code fragment, inferring the language from
// path's extension.
func Compile(pattern, path string) (*Pattern, error) {
	lg, ok := resolveLanguage(path)
	if !ok {
		return nil, fmt.Errorf("astgrep: cannot infer language from %q", path)
	}
	return compileWith(lg, pattern)
}

func compileWith(lg language, pattern string) (*Pattern, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("astgrep: empty pattern")
	}
	pp := lg.preprocess(pattern)
	tree, err := lg.parse(pp)
	if err != nil || tree == nil {
		return nil, fmt.Errorf("astgrep: parse pattern: %w", err)
	}
	root := tree.RootNode()
	if root == nil {
		return nil, fmt.Errorf("astgrep: empty pattern tree")
	}
	src := []byte(pp)
	sig := significant(root, src)
	pn := lg.convert(sig, src)
	if pn.kind == patMeta && (pn.mv.kind == metaMultiple || pn.mv.kind == metaMultiCapture) {
		return nil, fmt.Errorf("astgrep: pattern root cannot be an ellipsis metavariable")
	}
	rk := ""
	if pn.kind != patMeta {
		rk = pn.tsKind
	}
	return &Pattern{root: pn, lg: lg, rootKind: rk}, nil
}

// significant descends from the parse root to the node that represents the
// whole pattern, peeling off wrapper nodes (source_file, expression_statement,
// …) whose text is identical to the trimmed pattern. If descent lands on an
// error-recovery node, it steps into that node's sole named child when present.
func significant(root *gts.Node, src []byte) *gts.Node {
	target := strings.TrimSpace(string(src))
	cur := root
	for {
		var next *gts.Node
		for i := 0; i < cur.ChildCount(); i++ {
			c := cur.Child(i)
			if c == nil {
				continue
			}
			if strings.TrimSpace(c.Text(src)) == target {
				next = c
				break
			}
		}
		if next == nil || next == cur {
			break
		}
		cur = next
	}
	if cur.IsError() && cur.NamedChildCount() == 1 {
		if c := cur.NamedChild(0); c != nil {
			cur = c
		}
	}
	return cur
}

// convert turns a syntax node into a pattern node: a metavariable token becomes
// a hole, a leaf becomes a terminal matched by kind (+ text for named tokens),
// and an interior node becomes a kind-matched node with converted children.
func (lg language) convert(n *gts.Node, src []byte) patNode {
	text := n.Text(src)
	if mv, ok := extractMetaVar(text, lg.expando); ok {
		return patNode{kind: patMeta, mv: mv}
	}
	if n.ChildCount() == 0 {
		return patNode{kind: patTerminal, text: text, isNamed: n.IsNamed(), tsKind: n.Type(lg.lang)}
	}
	var kids []patNode
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil || c.IsMissing() {
			continue
		}
		kids = append(kids, lg.convert(c, src))
	}
	return patNode{kind: patInternal, tsKind: n.Type(lg.lang), children: kids}
}
