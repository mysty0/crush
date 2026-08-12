package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
)

func listModelsTestCoordinator(t *testing.T) *coordinator {
	t.Helper()
	cfg := &config.Config{
		Options: &config.Options{},
		Models:  map[config.SelectedModelType]config.SelectedModel{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"claude-code": {
				ID:   "claude-code",
				Name: "Claude Code (subscription)",
				Models: []catwalk.Model{
					{ID: "claude-opus-5"},
					{ID: "claude-haiku-4-5-20251001"},
				},
			},
			"yunwu": {
				ID:     "yunwu",
				Name:   "Yunwu",
				Models: []catwalk.Model{{ID: "deepseek-v3.2"}},
			},
		}),
	}
	return &coordinator{cfg: config.NewTestStore(cfg)}
}

func runListModels(t *testing.T, c *coordinator, params ListModelsParams) string {
	t.Helper()
	tool := c.listModelsTool()
	require.Equal(t, ListModelsToolName, tool.Info().Name)

	resp, err := runTypedTool(t, tool, params)
	require.NoError(t, err)
	return resp
}

func TestListModelsTool(t *testing.T) {
	t.Parallel()

	t.Run("lists every provider by default", func(t *testing.T) {
		t.Parallel()
		out := runListModels(t, listModelsTestCoordinator(t), ListModelsParams{})
		require.Contains(t, out, "claude-opus-5")
		require.Contains(t, out, "deepseek-v3.2")
		require.Contains(t, out, "3 model(s)")
	})

	t.Run("provider scopes the listing", func(t *testing.T) {
		t.Parallel()
		out := runListModels(t, listModelsTestCoordinator(t), ListModelsParams{Provider: "yunwu"})
		require.Contains(t, out, "deepseek-v3.2")
		require.NotContains(t, out, "claude-opus-5")
	})

	t.Run("filter matches substrings case-insensitively", func(t *testing.T) {
		t.Parallel()
		out := runListModels(t, listModelsTestCoordinator(t), ListModelsParams{Filter: "HAIKU"})
		require.Contains(t, out, "claude-haiku-4-5-20251001")
		require.NotContains(t, out, "claude-opus-5")
	})

	t.Run("no match explains why", func(t *testing.T) {
		t.Parallel()
		out := runListModels(t, listModelsTestCoordinator(t), ListModelsParams{Filter: "nonexistent"})
		require.Contains(t, out, "No models match")
	})
}

// runTypedTool invokes a typed fantasy tool with the given params.
func runTypedTool(t *testing.T, tool fantasy.AgentTool, params any) (string, error) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "test",
		Name:  tool.Info().Name,
		Input: string(input),
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
