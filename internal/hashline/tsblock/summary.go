package tsblock

import (
	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// SegmentKind distinguishes kept (verbatim) from elided (collapsed) line spans.
type SegmentKind int

const (
	// Kept lines are shown verbatim.
	Kept SegmentKind = iota
	// Elided lines are collapsed to a single "…" marker.
	Elided
)

// Segment is a contiguous run of 1-indexed lines of one kind.
type Segment struct {
	Kind  SegmentKind
	Start int // 1-based inclusive
	End   int // 1-based inclusive
}

// Summary is a structural view of a file: an ordered, gap-free partition of its
// lines into kept and elided segments, plus the total elided line count.
type Summary struct {
	Segments    []Segment
	ElidedLines int
	TotalLines  int
}

// Options tune the structural summary. Zero values fall back to defaults that
// mirror oh-my-pi's read.summarize.* settings.
type Options struct {
	MinBodyLines  int // min lines a construct must span before its body is elided (default 4)
	MinTotalLines int // files with fewer lines are read verbatim, not summarized (default 100)
	// UnfoldUntil is the target number of visible (kept) lines. Starting from a
	// fully folded view, elidable spans are unfolded breadth-first (outermost
	// first) until at least this many lines are visible. A file whose total
	// length is at or below UnfoldUntil is therefore shown verbatim. Default 400.
	UnfoldUntil int
	// UnfoldLimit is the hard ceiling on visible lines while unfolding: an
	// unfold whose revealed lines would exceed it is skipped so a single
	// oversized leaf cannot starve its siblings. Defaults to UnfoldUntil*2.
	UnfoldLimit int
}

// Default summary tuning.
const (
	defaultMinBodyLines  = 4
	defaultMinTotalLines = 100
	defaultUnfoldUntil   = 400
)

func (o Options) withDefaults() Options {
	if o.MinBodyLines <= 0 {
		o.MinBodyLines = defaultMinBodyLines
	}
	if o.MinTotalLines <= 0 {
		o.MinTotalLines = defaultMinTotalLines
	}
	if o.UnfoldUntil <= 0 {
		o.UnfoldUntil = defaultUnfoldUntil
	}
	if o.UnfoldLimit <= 0 {
		o.UnfoldLimit = o.UnfoldUntil * 2
	}
	return o
}

// elidableNode is a foldable region: the interior line span [lo, hi] that is
// hidden when the node is folded, plus the foldable regions nested inside it.
type elidableNode struct {
	lo, hi   int
	children []*elidableNode
}

func (n *elidableNode) lines() int { return n.hi - n.lo + 1 }

// Summarize parses text as the language inferred from path and returns a
// structural summary. Starting from a fully folded view (only outermost
// signatures visible), it unfolds declarations breadth-first until at least
// opts.UnfoldUntil lines are visible, so small files are shown verbatim and
// only genuinely large files keep bodies elided. It returns ok=false — meaning
// the caller should fall back to a raw read — when the language is unknown, the
// file has a syntax error, the file is below opts.MinTotalLines, or the file is
// larger than the budget yet has no foldable structure.
func Summarize(text, path string, opts Options) (Summary, bool) {
	opts = opts.withDefaults()

	total := lineCount(text)
	if total < opts.MinTotalLines {
		return Summary{}, false
	}

	entry := grammars.DetectLanguage(path)
	if entry == nil {
		return Summary{}, false
	}
	lang := entry.Language()
	if lang == nil {
		return Summary{}, false
	}
	parser := gts.NewParser(lang)
	tree, err := parser.Parse([]byte(text))
	if err != nil || tree == nil {
		return Summary{}, false
	}
	root := tree.RootNode()
	if root == nil || root.HasError() {
		return Summary{}, false
	}

	var roots []*elidableNode
	for i := 0; i < root.NamedChildCount(); i++ {
		if n := buildElidable(root.NamedChild(i), opts.MinBodyLines); n != nil {
			roots = append(roots, n)
		}
	}

	elided := selectFolded(roots, total, opts.UnfoldUntil, opts.UnfoldLimit)
	if len(elided) == 0 {
		// Everything fits within the budget: show the file verbatim. Bail out
		// only when the file is both large and unfoldable, so we never dump a
		// huge file that has no structure to collapse.
		if total > opts.UnfoldUntil {
			return Summary{}, false
		}
		return Summary{
			Segments:   []Segment{{Kind: Kept, Start: 1, End: total}},
			TotalLines: total,
		}, true
	}

	return Summary{
		Segments:    buildSegments(total, elided),
		ElidedLines: len(elided),
		TotalLines:  total,
	}, true
}

// buildElidable turns a syntax node into an elidable span tree: a node spanning
// at least minBody lines has a foldable interior (its lines minus the opening
// and closing line), and its foldable descendants become children so unfolding
// it reveals nested signatures while keeping their bodies folded. Returns nil
// for nodes too small to fold.
func buildElidable(n *gts.Node, minBody int) *elidableNode {
	if n == nil {
		return nil
	}
	start := int(n.StartPoint().Row) + 1
	end := int(n.EndPoint().Row) + 1
	if end-start+1 < minBody || start+1 > end-1 {
		return nil
	}
	node := &elidableNode{lo: start + 1, hi: end - 1}
	for i := 0; i < n.NamedChildCount(); i++ {
		if c := buildElidable(n.NamedChild(i), minBody); c != nil {
			node.children = append(node.children, c)
		}
	}
	return node
}

// selectFolded returns the set of elided line numbers after breadth-first
// unfolding. It starts with every root folded (minimum visible) and unfolds
// outermost-first — replacing a folded node with its children — until at least
// unfoldUntil lines are visible, skipping any unfold that would push the
// visible count past unfoldLimit. Files at or below the budget end up fully
// unfolded (empty result).
func selectFolded(roots []*elidableNode, total, unfoldUntil, unfoldLimit int) map[int]struct{} {
	folded := make(map[*elidableNode]struct{}, len(roots))
	foldedLines := 0
	queue := make([]*elidableNode, 0, len(roots))
	for _, r := range roots {
		folded[r] = struct{}{}
		foldedLines += r.lines()
		queue = append(queue, r)
	}
	visible := total - foldedLines
	for len(queue) > 0 && visible < unfoldUntil {
		node := queue[0]
		queue = queue[1:]
		if _, ok := folded[node]; !ok {
			continue
		}
		childLines := 0
		for _, c := range node.children {
			childLines += c.lines()
		}
		revealed := node.lines() - childLines
		if revealed < 0 {
			revealed = 0
		}
		if visible+revealed > unfoldLimit {
			continue
		}
		delete(folded, node)
		for _, c := range node.children {
			folded[c] = struct{}{}
			queue = append(queue, c)
		}
		visible += revealed
	}
	elided := map[int]struct{}{}
	for n := range folded {
		for ln := n.lo; ln <= n.hi; ln++ {
			elided[ln] = struct{}{}
		}
	}
	return elided
}

// buildSegments partitions [1, total] into contiguous kept/elided runs.
func buildSegments(total int, elided map[int]struct{}) []Segment {
	if total == 0 {
		return nil
	}
	kindOf := func(line int) SegmentKind {
		if _, ok := elided[line]; ok {
			return Elided
		}
		return Kept
	}
	var segs []Segment
	cur := Segment{Kind: kindOf(1), Start: 1, End: 1}
	for line := 2; line <= total; line++ {
		k := kindOf(line)
		if k == cur.Kind {
			cur.End = line
			continue
		}
		segs = append(segs, cur)
		cur = Segment{Kind: k, Start: line, End: line}
	}
	return append(segs, cur)
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	n := 1
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			n++
		}
	}
	if text[len(text)-1] == '\n' {
		n--
	}
	return n
}
