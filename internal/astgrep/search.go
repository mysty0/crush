package astgrep

import (
	"fmt"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

// Match is one structural match with its location, captured metavariables, and
// the nearest enclosing named symbol (function/method/class), for localization.
type Match struct {
	StartByte int
	EndByte   int
	StartLine int // 1-based
	StartCol  int // 1-based
	EndLine   int // 1-based
	Text      string
	Bindings  map[string]string // metavariable name -> captured source text
	Enclosing string            // nearest enclosing symbol name, "" if none
}

// Search compiles pattern (language inferred from path) and returns every match
// in source, in document order.
func Search(pattern, source, path string) ([]Match, error) {
	p, err := Compile(pattern, path)
	if err != nil {
		return nil, err
	}
	return p.Search(source)
}

// Search runs the compiled pattern over source.
func (p *Pattern) Search(source string) ([]Match, error) {
	tree, err := p.lg.parse(source)
	if err != nil || tree == nil {
		return nil, fmt.Errorf("astgrep: parse source: %w", err)
	}
	root := tree.RootNode()
	if root == nil {
		return nil, nil
	}
	src := []byte(source)
	var matches []Match
	var visit func(n *gts.Node)
	visit = func(n *gts.Node) {
		if p.rootKind == "" || kindsMatch(p.rootKind, n.Type(p.lg.lang)) {
			if e, ok := p.matchAt(n, src); ok {
				matches = append(matches, p.buildMatch(n, e, src))
			}
		}
		for i := 0; i < n.ChildCount(); i++ {
			if c := n.Child(i); c != nil {
				visit(c)
			}
		}
	}
	visit(root)
	return matches, nil
}

func (p *Pattern) buildMatch(n *gts.Node, e *env, src []byte) Match {
	start, end := n.StartPoint(), n.EndPoint()
	bindings := map[string]string{}
	for name, node := range e.single {
		bindings[name] = nodeText(node, src)
	}
	for name, nodes := range e.multi {
		bindings[name] = joinSource(nodes, src)
	}
	return Match{
		StartByte: int(n.StartByte()),
		EndByte:   int(n.EndByte()),
		StartLine: int(start.Row) + 1,
		StartCol:  int(start.Column) + 1,
		EndLine:   int(end.Row) + 1,
		Text:      n.Text(src),
		Bindings:  bindings,
		Enclosing: p.enclosingSymbol(n, src),
	}
}

// joinSource returns the original source spanning the captured nodes, so a
// $$$ capture preserves the between-node text (commas, spacing).
func joinSource(nodes []*gts.Node, src []byte) string {
	if len(nodes) == 0 {
		return ""
	}
	lo := int(nodes[0].StartByte())
	hi := int(nodes[len(nodes)-1].EndByte())
	if lo < 0 || hi > len(src) || lo > hi {
		return ""
	}
	return string(src[lo:hi])
}

// enclosingSymbol walks up from n to the nearest named code unit: a
// function/method/class declaration, or a binding (a `const f = () => …`, class
// field, object pair, or assignment) whose value is itself a function. A plain
// value binding is skipped so a match inside `const x = a.b.c` reports the
// enclosing function, not `x`.
func (p *Pattern) enclosingSymbol(n *gts.Node, src []byte) string {
	lang := p.lg.lang
	for a := n.Parent(); a != nil; a = a.Parent() {
		kind := a.Type(lang)
		switch {
		case isFuncOrClassKind(kind):
			// Anonymous functions (arrow_function, function_expression) have no
			// name field; declName returns "" and the walk continues to the
			// binding that names them.
			if name := declName(a, lang, src); name != "" {
				return name
			}
		case isBindingKind(kind) && bindsFunction(a, lang):
			if name := declName(a, lang, src); name != "" {
				return name
			}
		}
	}
	return ""
}

// isFuncOrClassKind reports whether a tree-sitter kind is a function- or
// class-like code unit.
func isFuncOrClassKind(kind string) bool {
	for _, s := range []string{"function", "method", "class", "interface", "struct", "constructor", "impl", "module", "namespace", "trait", "enum"} {
		if strings.Contains(kind, s) {
			return true
		}
	}
	return false
}

// isBindingKind reports whether a kind binds a value to a name.
func isBindingKind(kind string) bool {
	switch kind {
	case "variable_declarator", "field_definition", "public_field_definition",
		"field_declaration", "pair", "assignment", "assignment_expression",
		"short_var_declaration":
		return true
	}
	return false
}

// bindsFunction reports whether a binding node's value is a function, so that an
// anonymous function assigned to a name reports that name.
func bindsFunction(n *gts.Node, lang *gts.Language) bool {
	for _, field := range []string{"value", "right"} {
		if v := n.ChildByFieldName(field, lang); v != nil {
			k := v.Type(lang)
			if strings.Contains(k, "function") || strings.Contains(k, "arrow") || strings.Contains(k, "lambda") {
				return true
			}
		}
	}
	return false
}

// declName extracts a declaration's name from its name-ish field ("name",
// "key", "left"). It deliberately uses fields only — never a child scan — so an
// anonymous function does not pick up a parameter or body identifier.
func declName(n *gts.Node, lang *gts.Language, src []byte) string {
	for _, field := range []string{"name", "key", "left"} {
		if c := n.ChildByFieldName(field, lang); c != nil {
			switch c.Type(lang) {
			case "identifier", "property_identifier", "field_identifier",
				"type_identifier", "name", "constant", "shorthand_property_identifier":
				return c.Text(src)
			}
		}
	}
	return ""
}
