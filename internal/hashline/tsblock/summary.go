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
}

// Default summary tuning (matches oh-my-pi).
const (
	defaultMinBodyLines  = 4
	defaultMinTotalLines = 100
)

func (o Options) withDefaults() Options {
	if o.MinBodyLines <= 0 {
		o.MinBodyLines = defaultMinBodyLines
	}
	if o.MinTotalLines <= 0 {
		o.MinTotalLines = defaultMinTotalLines
	}
	return o
}

// Summarize parses text as the language inferred from path and returns a
// structural summary: top-level declarations keep their opening and closing
// lines while their bodies are elided; container declarations (e.g. classes)
// are recursed so nested signatures stay visible. It returns ok=false — meaning
// the caller should fall back to a raw read — when the language is unknown, the
// file has a syntax error, the file is below opts.MinTotalLines, or nothing was
// elided.
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

	elided := map[int]struct{}{}
	for i := 0; i < root.NamedChildCount(); i++ {
		collectElisions(root.NamedChild(i), opts.MinBodyLines, elided)
	}
	if len(elided) == 0 {
		return Summary{}, false
	}

	return Summary{
		Segments:    buildSegments(total, elided),
		ElidedLines: len(elided),
		TotalLines:  total,
	}, true
}

// collectElisions folds n: if n spans few lines it is kept whole; if it is a
// container (two or more child declarations that are themselves foldable) it
// recurses so child signatures stay visible; otherwise its interior body lines
// are elided.
func collectElisions(n *gts.Node, minBody int, elided map[int]struct{}) {
	if n == nil {
		return
	}
	start := int(n.StartPoint().Row) + 1
	end := int(n.EndPoint().Row) + 1
	if end-start+1 < minBody {
		return
	}

	var childDecls []*gts.Node
	for i := 0; i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c != nil && int(c.EndPoint().Row-c.StartPoint().Row)+1 >= minBody {
			childDecls = append(childDecls, c)
		}
	}

	if len(childDecls) >= 2 {
		// Container: keep this node's frame and each child's signature; recurse
		// so nested bodies are elided but their signatures stay visible.
		for _, c := range childDecls {
			collectElisions(c, minBody, elided)
		}
		return
	}

	// Leaf body: keep the opening line and closing line, elide the interior.
	for line := start + 1; line <= end-1; line++ {
		elided[line] = struct{}{}
	}
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
