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
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/assert"
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

// padTool is an inert stand-in for an MCP tool: it contributes its schema to the
// request and counts calls into it.
//
// Naming depends on the eval. In the edit eval a call here is a genuine misfire:
// the task is a single-file edit and no MCP tool is relevant. In the benefit eval
// the same call is usually legitimate auxiliary exploration (resolving a
// datasource before querying it), so that eval reports the figure as "aux calls"
// and its padTool answers as an empty-but-working tool rather than a broken one.
type padTool struct {
	info     fantasy.ToolInfo
	misfires *atomic.Int64
	// body, when set, is returned as a successful result instead of an error.
	// See Run for why the benefit eval needs that.
	body string
	opts fantasy.ProviderOptions
}

func (p *padTool) Info() fantasy.ToolInfo { return p.info }

func (p *padTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	p.misfires.Add(1)
	if p.body != "" {
		// Answer as a working tool. Two earlier versions of this both invalidated
		// benefit-eval runs by teaching the model something false about the world:
		// an error ("not available in this environment") convinced it the whole MCP
		// surface was dead, and then an empty result convinced it the datasource
		// did not exist. Either way it stopped before reaching the target tool and
		// scored as a failure while having reasoned correctly from what it was
		// told. Auxiliary tools therefore need plausible, coherent answers.
		return fantasy.NewTextResponse(p.body), nil
	}
	// The edit eval wants the opposite: its tasks are single-file edits where no
	// MCP tool is relevant, so a call here is a genuine misfire and saying so
	// steers the model back to the file tools.
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
	return mcpPadToolsFor(t, strings.TrimSpace(os.Getenv("CRUSH_EDIT_EVAL_MCP_PAD")), misfires)
}

// mcpPadToolsFor is mcpPadTools with the selection passed explicitly.
func mcpPadToolsFor(t *testing.T, spec string, misfires *atomic.Int64) ([]fantasy.AgentTool, int) {
	t.Helper()
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

// TestToolSearchShrinksCapturedSurface measures the deferral saving on the real
// 98-schema surface captured from a production request, rather than on synthetic
// fixtures. This is the number the feature exists to move, so it is worth
// guarding against regression.
func TestToolSearchShrinksCapturedSurface(t *testing.T) {
	t.Parallel()

	var misfires atomic.Int64
	eagerTools, eagerBytes := mcpPadToolsFor(t, "all", &misfires)
	require.Len(t, eagerTools, 98)

	var deferredBytes, kept int
	for _, tool := range eagerTools {
		full, err := json.Marshal(tool.Info())
		require.NoError(t, err)
		if !tools.WorthDeferring(tool) {
			kept++
			deferredBytes += len(full)
			continue
		}
		stub, err := json.Marshal(tools.NewDeferredTool(tool).Info())
		require.NoError(t, err)
		deferredBytes += len(stub)
	}

	require.Less(t, deferredBytes, eagerBytes/2,
		"deferral should more than halve the captured MCP surface")
	t.Logf("captured surface: eager %d B -> deferred %d B (%.2fx smaller); %d kept eager",
		eagerBytes, deferredBytes, float64(eagerBytes)/float64(deferredBytes), kept)
}

// TestContainsAnswerHandlesDigitGrouping guards the scoring rule. A plain
// substring check scored a correct "**8,241**" as a failure against the token
// "8241", so digit separators are normalised -- but only between digits, or
// tokens like "dpl-9f4c21" would be mangled into false positives.
func TestContainsAnswerHandlesDigitGrouping(t *testing.T) {
	t.Parallel()

	assert.True(t, containsAnswer("The depth is **8,241**.", "8241"))
	assert.True(t, containsAnswer("restarted 17 times", "17"))
	assert.True(t, containsAnswer("limit of 3172Mi", "3172Mi"))
	assert.True(t, containsAnswer("deploy_id=dpl-9f4c21", "dpl-9f4c21"))
	assert.True(t, containsAnswer("key is `zodStrictParseV7`", "zodStrictParseV7"))
	assert.True(t, containsAnswer("id /drizzle-team/drizzle-orm-v42", "/drizzle-team/drizzle-orm-v42"))
	assert.True(t, containsAnswer("rate is 0.0271", "0.0271"))

	// Separators inside a non-numeric token must not be stripped away.
	assert.False(t, containsAnswer("inc 4482 zz", "inc-4482-zz"))
	assert.False(t, containsAnswer("I could not retrieve the value", "8241"))
	assert.False(t, containsAnswer("the threshold is 91.6", "91.5"))
}
