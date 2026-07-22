package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/compressd"
)

// RetrieveFullOutputToolName is the name the model calls to recover the
// original, uncompressed content of a tool result that was replaced with
// a short summary + compressed text by the tool-output compression step
// (see agent.sessionAgent's PrepareStep).
const RetrieveFullOutputToolName = "retrieve_full_output"

//go:embed retrieve_full_output.md
var retrieveFullOutputDescription string

// RetrieveFullOutputParams is the input for the retrieve_full_output tool.
type RetrieveFullOutputParams struct {
	// ID is the placeholder ID quoted in a compressed tool-result message.
	ID string `json:"id" description:"The placeholder id quoted in a compressed tool output message"`
}

// NewRetrieveFullOutputTool returns a tool that looks up the original
// content stashed by tool-output compression for the current session.
func NewRetrieveFullOutputTool(store *compressd.RetrievalStore) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		RetrieveFullOutputToolName,
		retrieveFullOutputDescription,
		func(ctx context.Context, params RetrieveFullOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return fantasy.NewTextErrorResponse("missing id"), nil
			}
			sessionID := GetSessionFromContext(ctx)
			content, ok := store.Get(sessionID, params.ID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("no stored output found for id %q", params.ID)), nil
			}
			return fantasy.NewTextResponse(content), nil
		},
	)
}
