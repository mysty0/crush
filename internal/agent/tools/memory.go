package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/permission"
)

const (
	RememberToolName = "Remember"
	RecallToolName   = "Recall"
	ForgetToolName   = "Forget"
)

//go:embed remember.md
var rememberDescription string

//go:embed recall.md
var recallDescription string

//go:embed forget.md
var forgetDescription string

// memoryScope maps a tool's scope argument to a store scope key.
func memoryScope(arg, projectScope string) string {
	if strings.EqualFold(strings.TrimSpace(arg), "global") {
		return memory.ScopeGlobal
	}
	return projectScope
}

// RememberParams is the input to the Remember tool.
type RememberParams struct {
	Content    string  `json:"content" description:"A durable, self-contained fact worth recalling in a future session: a project convention, a build/test command, where something lives, a decision, or a user preference. One sentence. Not transient task state."`
	Kind       string  `json:"kind,omitempty" description:"One of: fact, preference, convention, decision. Defaults to fact."`
	Importance float64 `json:"importance,omitempty" description:"0..1 salience; higher is recalled first. Defaults to 0.5."`
	Scope      string  `json:"scope,omitempty" description:"'project' (default) for repo-specific facts, or 'global' for user preferences that apply everywhere."`
}

// NewRememberTool stores a durable fact in long-term memory. Storing a
// memory persists it across every future session, so it goes through the
// same permission gate as any other mutating tool (and is blocked
// outright while Plan Mode is active, see permission.PlanModeBlocksTool)
// rather than writing silently -- without this, content the model reads
// from an untrusted source (a fetched page, a file, an MCP resource)
// could plant an instruction that gets auto-injected into every future
// turn with no user confirmation.
func NewRememberTool(store *memory.Store, permissions permission.Service, workingDir string, maxPerScope int) fantasy.AgentTool {
	projectScope := memory.ProjectScope(workingDir)
	return fantasy.NewAgentTool(
		RememberToolName,
		rememberDescription,
		func(ctx context.Context, params RememberParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(params.Content) == "" {
				return fantasy.NewTextErrorResponse("content is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session_id is required")
			}
			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        workingDir,
					ToolCallID:  call.ID,
					ToolName:    RememberToolName,
					Action:      "remember",
					Description: fmt.Sprintf("Remember: %s", strings.TrimSpace(params.Content)),
					Params:      params,
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			_, created, err := store.Remember(ctx, memory.RememberParams{
				Scope:       memoryScope(params.Scope, projectScope),
				Content:     strings.TrimSpace(params.Content),
				Kind:        params.Kind,
				Importance:  params.Importance,
				Source:      "tool",
				MaxPerScope: maxPerScope,
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("could not store memory: %v", err)), nil
			}
			if created {
				return fantasy.NewTextResponse("Remembered."), nil
			}
			return fantasy.NewTextResponse("Merged into an existing memory."), nil
		},
	)
}

// RecallParams is the input to the Recall tool.
type RecallParams struct {
	Query string `json:"query" description:"What you want to remember about this project or the user — a topic, question, or keywords."`
}

// NewRecallTool searches long-term memory. Recall also happens automatically;
// this tool is for explicit lookups.
func NewRecallTool(store *memory.Store, workingDir string) fantasy.AgentTool {
	projectScope := memory.ProjectScope(workingDir)
	return fantasy.NewAgentTool(
		RecallToolName,
		recallDescription,
		func(ctx context.Context, params RecallParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(params.Query) == "" {
				return fantasy.NewTextErrorResponse("query is required"), nil
			}
			hits, err := store.Recall(ctx, []string{projectScope, memory.ScopeGlobal}, params.Query, 8)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("could not recall: %v", err)), nil
			}
			if len(hits) == 0 {
				return fantasy.NewTextResponse("No relevant memories."), nil
			}
			var b strings.Builder
			for _, h := range hits {
				fmt.Fprintf(&b, "- %s\n", h.Content)
			}
			return fantasy.NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
		},
	)
}

// ForgetParams is the input to the Forget tool.
type ForgetParams struct {
	Target string `json:"target" description:"A memory id, or text describing the memory to remove (e.g. a fact that turned out to be wrong)."`
	Scope  string `json:"scope,omitempty" description:"'project' (default) or 'global'."`
}

// NewForgetTool removes a memory the agent (or user) found to be wrong or
// stale. Gated the same way as NewRememberTool -- deleting a memory is
// also a persistent mutation worth confirming.
func NewForgetTool(store *memory.Store, permissions permission.Service, workingDir string) fantasy.AgentTool {
	projectScope := memory.ProjectScope(workingDir)
	return fantasy.NewAgentTool(
		ForgetToolName,
		forgetDescription,
		func(ctx context.Context, params ForgetParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(params.Target) == "" {
				return fantasy.NewTextErrorResponse("target is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session_id is required")
			}
			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        workingDir,
					ToolCallID:  call.ID,
					ToolName:    ForgetToolName,
					Action:      "forget",
					Description: fmt.Sprintf("Forget: %s", strings.TrimSpace(params.Target)),
					Params:      params,
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			n, err := store.Forget(ctx, memoryScope(params.Scope, projectScope), strings.TrimSpace(params.Target))
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("could not forget: %v", err)), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Forgot %d memor%s.", n, plural(n))), nil
		},
	)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
