package astgrep

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

// matchRes is the outcome of comparing one pattern node with one candidate.
type matchRes int

const (
	mMatched       matchRes = iota // pattern node consumed a candidate node
	mSkipCandidate                 // candidate is trivial; skip it, keep the goal
	mNoMatch                       // hard failure
)

// env holds metavariable bindings accumulated during a match attempt.
type env struct {
	single map[string]*gts.Node
	multi  map[string][]*gts.Node
	src    []byte
	lang   *gts.Language
}

func newEnv(src []byte, lang *gts.Language) *env {
	return &env{single: map[string]*gts.Node{}, multi: map[string][]*gts.Node{}, src: src, lang: lang}
}

// clone returns a shallow copy used for lookahead probes so a failed trial
// binding never leaks into the real environment.
func (e *env) clone() *env {
	c := newEnv(e.src, e.lang)
	for k, v := range e.single {
		c.single[k] = v
	}
	for k, v := range e.multi {
		c.multi[k] = v
	}
	return c
}

// bind records a single-node capture, enforcing consistency: a repeated
// metavariable must match identical source text.
func (e *env) bind(name string, n *gts.Node) bool {
	if name == "" {
		return true
	}
	if prev, ok := e.single[name]; ok {
		return nodeText(prev, e.src) == nodeText(n, e.src)
	}
	e.single[name] = n
	return true
}

func (e *env) bindMulti(mv metaVar, nodes []*gts.Node) {
	if mv.kind == metaMultiCapture {
		e.multi[mv.name] = trimTrailingUnnamed(nodes)
	}
}

// matchNode compares a single pattern node with a candidate node under "smart"
// strictness. Ellipsis metavariables are only meaningful inside a child
// sequence and are handled by matchChildren, not here.
func (e *env) matchNode(g patNode, cand *gts.Node) matchRes {
	switch g.kind {
	case patTerminal:
		return e.matchTerminal(g, cand)
	case patMeta:
		return e.matchMeta(g.mv, cand)
	case patInternal:
		if !kindsMatch(g.tsKind, cand.Type(e.lang)) {
			if !cand.IsNamed() {
				return mSkipCandidate
			}
			return mNoMatch
		}
		if e.matchChildren(g.children, cand) {
			return mMatched
		}
		return mNoMatch
	}
	return mNoMatch
}

// matchTerminal implements ast-grep's smart per-token rule: kinds must match
// and, for named tokens, text must be equal; an unmatched unnamed candidate is
// skipped as trivia; an unmatched named candidate fails.
func (e *env) matchTerminal(g patNode, cand *gts.Node) matchRes {
	if kindsMatch(g.tsKind, cand.Type(e.lang)) && (!g.isNamed || g.text == nodeText(cand, e.src)) {
		return mMatched
	}
	if !cand.IsNamed() {
		return mSkipCandidate
	}
	return mNoMatch
}

// matchMeta binds (or drops) a single-node metavariable.
func (e *env) matchMeta(mv metaVar, cand *gts.Node) matchRes {
	switch mv.kind {
	case metaCapture:
		if mv.named && !cand.IsNamed() {
			return mNoMatch
		}
		if !e.bind(mv.name, cand) {
			return mNoMatch
		}
		return mMatched
	case metaDropped:
		if mv.named && !cand.IsNamed() {
			return mNoMatch
		}
		return mMatched
	}
	return mNoMatch
}

// matchChildren aligns a pattern's child sequence against a candidate's
// children, skipping trivial (unnamed) candidates, ignoring trailing
// candidates, and expanding $$$ ellipses greedily up to the next pattern node.
func (e *env) matchChildren(goals []patNode, cand *gts.Node) bool {
	cands := childrenOf(cand)
	gi, ci := 0, 0
	for gi < len(goals) {
		g := goals[gi]

		if isEllipsis(g) {
			gi++
			if gi >= len(goals) {
				e.bindMulti(g.mv, cands[ci:])
				ci = len(cands)
				continue
			}
			next := goals[gi]
			var captured []*gts.Node
			for ci < len(cands) {
				if isEllipsis(next) { // adjacent ellipsis: consume exactly one
					break
				}
				probe := e.clone()
				if probe.matchNode(next, cands[ci]) == mMatched {
					break
				}
				captured = append(captured, cands[ci])
				ci++
			}
			e.bindMulti(g.mv, captured)
			continue
		}

		if ci >= len(cands) {
			// Remaining goals can only match if they are empty-capable ellipses.
			for ; gi < len(goals); gi++ {
				if !isEllipsis(goals[gi]) {
					return false
				}
				e.bindMulti(goals[gi].mv, nil)
			}
			return true
		}

		switch e.matchNode(g, cands[ci]) {
		case mMatched:
			gi++
			ci++
		case mSkipCandidate:
			ci++
		default:
			return false
		}
	}
	// Smart strictness ignores any trailing candidate nodes.
	return true
}

// Match reports whether the pattern matches the given node, returning the
// bindings captured on success.
func (p *Pattern) matchAt(cand *gts.Node, src []byte) (*env, bool) {
	e := newEnv(src, p.lg.lang)
	if e.matchNode(p.root, cand) == mMatched {
		return e, true
	}
	return nil, false
}

// kindsMatch is tree-sitter kind equality with ERROR acting as a wildcard on
// the pattern (goal) side.
func kindsMatch(goalKind, candKind string) bool {
	return goalKind == candKind || goalKind == "ERROR"
}

func isEllipsis(g patNode) bool {
	return g.kind == patMeta && (g.mv.kind == metaMultiple || g.mv.kind == metaMultiCapture)
}

func childrenOf(n *gts.Node) []*gts.Node {
	m := n.ChildCount()
	out := make([]*gts.Node, 0, m)
	for i := 0; i < m; i++ {
		if c := n.Child(i); c != nil {
			out = append(out, c)
		}
	}
	return out
}

func trimTrailingUnnamed(ns []*gts.Node) []*gts.Node {
	for len(ns) > 0 && !ns[len(ns)-1].IsNamed() {
		ns = ns[:len(ns)-1]
	}
	return ns
}

func nodeText(n *gts.Node, src []byte) string {
	return strings.TrimSpace(n.Text(src))
}
