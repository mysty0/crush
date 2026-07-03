// Package workflow implements a sandboxed Lua workflow engine used to
// run multi-phase, multi-agent research/automation flows (e.g. deep
// research: scope -> search -> fetch -> verify -> synthesize).
//
// The engine itself has no knowledge of Crush's sessions, permissions,
// or model configuration: callers provide a Runner implementation that
// executes individual sub-agent turns and structured-output coercions.
// This keeps the engine independently testable and free of import
// cycles with internal/agent.
package workflow

// Phase describes one stage of a workflow's pipeline, shown in
// progress UI (e.g. "Scope", "Search", "Fetch", "Verify",
// "Synthesize").
type Phase struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// Workflow is a discovered, runnable Lua workflow definition.
type Workflow struct {
	// Name is the workflow identifier passed via Workflow({name=...}).
	Name string
	// Description is a short one-line summary shown in tool
	// descriptions.
	Description string
	// WhenToUse guides the model on when to invoke this workflow.
	WhenToUse string
	// Phases describes the pipeline stages for documentation/UI
	// purposes. Purely informational; the script drives the actual
	// phase() calls at runtime.
	Phases []Phase
	// Script is the Lua source implementing the workflow.
	Script string
	// Source identifies where the workflow was discovered from
	// (e.g. "built-in").
	Source string
}
