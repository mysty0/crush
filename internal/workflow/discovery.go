package workflow

import (
	"embed"
	"strings"
)

//go:embed builtin/*.lua
var builtinFS embed.FS

// builtinWorkflows describes metadata for each embedded workflow in
// builtin/. This is kept as a small Go table (rather than parsed from
// the Lua source, e.g. via magic comments) because it's simple,
// type-safe, and there is exactly one built-in workflow today.
var builtinWorkflows = []Workflow{
	{
		Name:        "deep-research",
		Description: "Deep research harness -- fan-out web searches, fetch sources, adversarially verify claims, synthesize a cited report.",
		WhenToUse:   "When the user wants a deep, multi-source, fact-checked research report on any topic. Before invoking, check if the question is specific enough to research directly -- if underspecified (e.g. \"what car to buy\" without budget/use-case/region), ask 2-3 clarifying questions to narrow scope. Then pass the refined question as args, weaving the answers in.",
		Phases: []Phase{
			{Title: "Scope", Detail: "Decompose question (from args) into search angles"},
			{Title: "Search", Detail: "Parallel WebSearch agents, one per angle"},
			{Title: "Fetch", Detail: "URL-dedup, fetch top sources, extract falsifiable claims"},
			{Title: "Verify", Detail: "3-vote adversarial verification per claim (need 2/3 refutes to kill)"},
			{Title: "Synthesize", Detail: "Merge semantic dupes, rank by confidence, cite sources"},
		},
		Source: "built-in",
	},
}

// Discover returns every available workflow. For now this is just the
// embedded built-ins; user/project-supplied workflow directories are
// not yet supported (see the design note in AGENTS.md/workflow docs).
func Discover() ([]*Workflow, error) {
	out := make([]*Workflow, 0, len(builtinWorkflows))
	for _, w := range builtinWorkflows {
		w := w
		script, err := builtinFS.ReadFile("builtin/" + w.Name + ".lua")
		if err != nil {
			return nil, err
		}
		w.Script = string(script)
		out = append(out, &w)
	}
	return out, nil
}

// Find returns the named workflow, or nil if it isn't known.
func Find(name string) (*Workflow, error) {
	workflows, err := Discover()
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	for _, w := range workflows {
		if w.Name == name {
			return w, nil
		}
	}
	return nil, nil
}
