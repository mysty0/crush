package agent

// Tool-surface padding for the edit eval.
//
// Crush sends every configured MCP tool schema on every request. On this
// machine that is 98 schemas / ~70KB, which is ~1.5x Claude Code's whole
// prefix. The tokens are cheap once they are cache reads, but they also
// occupy the model's attention, so the open question is whether carrying a
// tool surface you do not use in the current project costs task accuracy.
//
// To answer that with a paired A/B, the eval can pad its tool list with the
// real captured schemas (testdata/mcp_surface.json — byte-identical to what
// Crush sends in production, so the measurement reflects the real prefix
// rather than a synthetic stand-in). The padded tools are inert: calling one
// is a *misfire*, which the scoreboard counts rather than serves, since
// "the surface lured the model away from the task" is exactly one of the
// harms being measured.
//
//   CRUSH_EDIT_EVAL_MCP_PAD=none      no padding (default)
//   CRUSH_EDIT_EVAL_MCP_PAD=all       all 98 schemas
//   CRUSH_EDIT_EVAL_MCP_PAD=grafana   only that server's schemas (repeatable:
//                                     "grafana,playwright")
//   CRUSH_EDIT_EVAL_MCP_PAD=unused    servers with zero recorded calls in any
//                                     project (playwright, trello)

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// unusedServers have zero recorded tool calls across every project DB on this
// machine, so they are pure prefix cost wherever they are configured.
var unusedServers = []string{"playwright", "trello"}

type mcpSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// padTool is an inert stand-in for an MCP tool: it contributes its schema to
// the request and records a misfire if the model ever calls it.
type padTool struct {
	info     fantasy.ToolInfo
	misfires *atomic.Int64
	opts     fantasy.ProviderOptions
}

func (p *padTool) Info() fantasy.ToolInfo { return p.info }

func (p *padTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	p.misfires.Add(1)
	return fantasy.NewTextErrorResponse(fmt.Sprintf(
		"%s is not available in this environment; complete the task with the file tools",
		call.Name,
	)), nil
}

func (p *padTool) ProviderOptions() fantasy.ProviderOptions     { return p.opts }
func (p *padTool) SetProviderOptions(o fantasy.ProviderOptions) { p.opts = o }

// loadMCPSurface reads the captured production schemas.
func loadMCPSurface(t *testing.T) []mcpSchema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "mcp_surface.json"))
	require.NoError(t, err)
	var out []mcpSchema
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// mcpPadTools returns the inert tools selected by CRUSH_EDIT_EVAL_MCP_PAD,
// along with the byte size of the schemas they add.
func mcpPadTools(t *testing.T, misfires *atomic.Int64) ([]fantasy.AgentTool, int) {
	t.Helper()
	spec := strings.TrimSpace(os.Getenv("CRUSH_EDIT_EVAL_MCP_PAD"))
	if spec == "" || spec == "none" {
		return nil, 0
	}

	var want map[string]bool
	switch spec {
	case "all":
		// nil predicate: keep everything.
	case "unused":
		want = map[string]bool{}
		for _, s := range unusedServers {
			want[s] = true
		}
	default:
		want = map[string]bool{}
		for _, s := range strings.Split(spec, ",") {
			if s = strings.TrimSpace(s); s != "" {
				want[s] = true
			}
		}
	}

	var out []fantasy.AgentTool
	bytes := 0
	for _, s := range loadMCPSurface(t) {
		if want != nil && !want[mcpServerOf(s.Name)] {
			continue
		}
		params, _ := s.InputSchema["properties"].(map[string]any)
		var required []string
		if raw, ok := s.InputSchema["required"].([]any); ok {
			for _, r := range raw {
				if str, ok := r.(string); ok {
					required = append(required, str)
				}
			}
		}
		out = append(out, &padTool{
			info: fantasy.ToolInfo{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  params,
				Required:    required,
			},
			misfires: misfires,
		})
		if b, err := json.Marshal(s); err == nil {
			bytes += len(b)
		}
	}
	return out, bytes
}

// mcpServerOf extracts the server name from an "mcp__<server>__<tool>" name.
func mcpServerOf(tool string) string {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return ""
	}
	server, _, _ := strings.Cut(rest, "__")
	return server
}
