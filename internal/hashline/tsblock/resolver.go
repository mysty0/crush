// Package tsblock provides a tree-sitter-backed [hashline.BlockResolver] for
// hashline's block operations (SWAP.BLK / DEL.BLK / INS.BLK.POST).
//
// It is the single import site for the pure-Go tree-sitter runtime, isolating
// that dependency (and its embedded grammars) from the rest of the codebase.
// The runtime and grammars are CGO-free, so the CGO_ENABLED=0 build is
// preserved.
package tsblock

import (
	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/charmbracelet/crush/internal/hashline"
)

// Resolver resolves the syntactic block beginning on a given line via
// tree-sitter. It is stateless and safe for concurrent use.
type Resolver struct{}

// New returns a tree-sitter block resolver.
func New() *Resolver { return &Resolver{} }

var _ hashline.BlockResolver = (*Resolver)(nil)

// ResolveBlock parses req.Text as the language inferred from req.Path and
// returns the 1-indexed inclusive line span of the outermost syntactic block
// that begins on req.Line. It returns ok=false when the language is unknown,
// no node begins on that line, or the resolved block contains a syntax error.
//
// Single-line spans are returned as-is; the caller (hashline.ResolveBlockEdits)
// treats a single-line block as a mis-anchor and rejects/lowers accordingly.
func (r *Resolver) ResolveBlock(req hashline.BlockRequest) (hashline.BlockSpan, bool) {
	if req.Line < 1 {
		return hashline.BlockSpan{}, false
	}
	entry := grammars.DetectLanguage(req.Path)
	if entry == nil {
		return hashline.BlockSpan{}, false
	}
	lang := entry.Language()
	if lang == nil {
		return hashline.BlockSpan{}, false
	}

	parser := gts.NewParser(lang)
	tree, err := parser.Parse([]byte(req.Text))
	if err != nil || tree == nil {
		return hashline.BlockSpan{}, false
	}

	node := outermostNamedNodeAtRow(tree.RootNode(), uint32(req.Line-1))
	if node == nil || node.HasError() {
		return hashline.BlockSpan{}, false
	}
	return hashline.BlockSpan{
		Start: int(node.StartPoint().Row) + 1,
		End:   int(node.EndPoint().Row) + 1,
	}, true
}

// outermostNamedNodeAtRow returns the largest-span named node whose start row
// equals row, excluding the tree root. Nodes that begin at the same position
// are nested, so the largest span is the shallowest (outermost) ancestor —
// which is exactly the construct that "begins on line N".
//
// The walk prunes any subtree whose range cannot contain a node starting on
// row, so it stays cheap even on large files.
func outermostNamedNodeAtRow(root *gts.Node, row uint32) *gts.Node {
	var best *gts.Node
	var walk func(n *gts.Node, isRoot bool)
	walk = func(n *gts.Node, isRoot bool) {
		if n == nil {
			return
		}
		start := n.StartPoint().Row
		end := n.EndPoint().Row
		if start > row || end < row {
			return
		}
		if !isRoot && n.IsNamed() && start == row {
			if best == nil || (end-start) > (best.EndPoint().Row-best.StartPoint().Row) {
				best = n
			}
		}
		for i := 0; i < n.ChildCount(); i++ {
			walk(n.Child(i), false)
		}
	}
	walk(root, true)
	return best
}
