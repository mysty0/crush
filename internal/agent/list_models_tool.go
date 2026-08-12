package agent

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/config"
)

// ListModelsToolName is the tool that returns the models selectable via
// the "model" parameter of the agent, Workflow, and agentic_fetch tools.
const ListModelsToolName = "list_models"

//go:embed templates/list_models.md
var listModelsDescription string

// ListModelsParams are the parameters for the list_models tool.
type ListModelsParams struct {
	Provider string `json:"provider,omitempty" description:"Only list models from this provider ID."`
	Filter   string `json:"filter,omitempty" description:"Only list models whose ID contains this substring (case-insensitive)."`
}

// listModelsTool implements the list_models tool. The full model catalog
// used to be appended to the agent/Workflow/agentic_fetch descriptions,
// where it sat in the cached prompt prefix of every request and, on a
// config with many providers, dwarfed every other tool schema combined.
// Serving it as a tool result instead means it costs nothing until the
// model actually needs a model it was not told about.
func (c *coordinator) listModelsTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		ListModelsToolName,
		listModelsDescription,
		func(ctx context.Context, params ListModelsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			providers := slices.SortedFunc(
				slices.Values(c.cfg.Config().EnabledProviders()),
				func(a, b config.ProviderConfig) int { return strings.Compare(a.ID, b.ID) },
			)

			filter := strings.ToLower(params.Filter)
			var b strings.Builder
			matches := 0

			for _, providerCfg := range providers {
				if params.Provider != "" && providerCfg.ID != params.Provider {
					continue
				}

				var models []string
				for _, m := range providerCfg.Models {
					if filter != "" && !strings.Contains(strings.ToLower(m.ID), filter) {
						continue
					}
					models = append(models, m.ID)
				}
				if len(models) == 0 {
					continue
				}

				matches += len(models)
				name := providerCfg.Name
				if name == "" {
					name = providerCfg.ID
				}
				fmt.Fprintf(&b, "\n%s (%s):\n", name, providerCfg.ID)
				for _, id := range models {
					fmt.Fprintf(&b, "- %s\n", id)
				}
			}

			if matches == 0 {
				return fantasy.NewTextResponse(describeNoModelMatches(params)), nil
			}

			return fantasy.NewTextResponse(fmt.Sprintf(
				"%d model(s) available for the `model` parameter:\n%s", matches, b.String(),
			)), nil
		},
	)
}

func describeNoModelMatches(params ListModelsParams) string {
	switch {
	case params.Provider != "" && params.Filter != "":
		return fmt.Sprintf("No models in provider %q match %q.", params.Provider, params.Filter)
	case params.Provider != "":
		return fmt.Sprintf("No models available in provider %q.", params.Provider)
	case params.Filter != "":
		return fmt.Sprintf("No models match %q.", params.Filter)
	default:
		return "No models available."
	}
}
