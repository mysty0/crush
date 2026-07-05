package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
	Model  string `json:"model,omitempty" description:"Optional. The ID of the model to run this task on, chosen from the list of available models in this tool's description. Omit to use the default model. Prefer a more capable model for hard, reasoning-heavy tasks and a smaller, faster model for routine or well-scoped tasks."`
}

const (
	AgentToolName = "agent"
)

// taskAgentKey is the registry key for a task agent, uniquely identifying
// the provider + model it runs on so two providers exposing the same model
// ID do not collide.
func taskAgentKey(providerID, modelID string) string {
	return providerID + "/" + modelID
}

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	taskPromptTmpl, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	defaultAgent, err := c.buildAgent(ctx, taskPromptTmpl, agentCfg, true)
	if err != nil {
		return nil, err
	}

	// Register the default (configured large) task agent under its model
	// key and prune idle, non-default per-model instances left over from a
	// previous model configuration. Busy instances are always kept so an
	// in-flight sub-agent turn stays steerable/cancelable even after the
	// user switches models.
	largeCfg := c.cfg.Config().Models[config.SelectedModelTypeLarge]
	defaultKey := taskAgentKey(largeCfg.Provider, largeCfg.Model)
	c.defaultTaskModelID.Set(defaultKey)
	c.taskAgents.Set(defaultKey, defaultAgent)
	for key, existing := range c.taskAgents.Seq2() {
		if key != defaultKey && (existing == nil || !existing.IsBusy()) {
			c.taskAgents.Del(key)
		}
	}

	description := agentToolDescription + c.availableModelsDescription()

	return fantasy.NewParallelAgentTool(
		AgentToolName,
		description,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			selected := defaultAgent
			if params.Model != "" {
				a, err := c.taskAgentForModel(ctx, agentCfg, params.Model)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				selected = a
			}

			return c.runSubAgent(ctx, subAgentParams{
				Agent:          selected,
				SessionID:      sessionID,
				AgentMessageID: agentMessageID,
				ToolCallID:     call.ID,
				Prompt:         params.Prompt,
				SessionTitle:   "New Agent Session",
			})
		},
	), nil
}

// taskAgentForModel returns the task agent that runs on the given model ID,
// building and caching it on first use. The model must belong to an enabled
// provider; otherwise an error naming the valid model IDs is returned so the
// LLM can retry with a supported one.
func (c *coordinator) taskAgentForModel(ctx context.Context, agentCfg config.Agent, modelID string) (SessionAgent, error) {
	selected, ok := c.resolveTaskModel(modelID)
	if !ok {
		return nil, fmt.Errorf(
			"unknown model %q; choose one of the available model IDs: %s",
			modelID, strings.Join(c.availableModelIDs(), ", "),
		)
	}

	key := taskAgentKey(selected.Provider, selected.Model)
	if existing, ok := c.taskAgents.Get(key); ok && existing != nil {
		return existing, nil
	}

	taskPromptTmpl, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}
	built, err := c.buildAgentWithModel(ctx, taskPromptTmpl, agentCfg, selected)
	if err != nil {
		return nil, err
	}
	c.taskAgents.Set(key, built)
	return built, nil
}

// resolveTaskModel resolves a model ID the LLM passed into a SelectedModel
// runnable by the "agent" tool. If the ID matches a configured slot (large
// or small) the slot's full configuration is reused so the user's tuning
// (thinking, reasoning effort, temperature, etc.) is honored. Otherwise a
// SelectedModel is synthesized from the first enabled provider that exposes
// the model, using the same defaults the app applies to a fresh selection.
func (c *coordinator) resolveTaskModel(modelID string) (config.SelectedModel, bool) {
	cfg := c.cfg.Config()

	for _, slot := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
		if m, ok := cfg.Models[slot]; ok && m.Model == modelID {
			if _, ok := cfg.Providers.Get(m.Provider); ok {
				return m, true
			}
		}
	}

	for _, providerCfg := range cfg.EnabledProviders() {
		for _, m := range providerCfg.Models {
			if m.ID != modelID {
				continue
			}
			return config.SelectedModel{
				Provider:        providerCfg.ID,
				Model:           m.ID,
				MaxTokens:       m.DefaultMaxTokens,
				ReasoningEffort: m.DefaultReasoningEffort,
			}, true
		}
	}

	return config.SelectedModel{}, false
}

// availableModelIDs returns the sorted, de-duplicated set of model IDs the
// "agent" tool can dispatch to (every model on an enabled provider).
func (c *coordinator) availableModelIDs() []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, providerCfg := range c.cfg.Config().EnabledProviders() {
		for _, m := range providerCfg.Models {
			if _, ok := seen[m.ID]; ok {
				continue
			}
			seen[m.ID] = struct{}{}
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// availableModelsDescription renders the list of selectable models, grouped
// by provider, to append to the tool description so the LLM knows which IDs
// it may pass in the "model" parameter.
func (c *coordinator) availableModelsDescription() string {
	providers := c.cfg.Config().EnabledProviders()
	if len(providers) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nAvailable models for the `model` parameter, grouped by provider:\n")
	for _, providerCfg := range providers {
		if len(providerCfg.Models) == 0 {
			continue
		}
		name := providerCfg.Name
		if name == "" {
			name = providerCfg.ID
		}
		b.WriteString(fmt.Sprintf("\n%s:\n", name))
		for _, m := range providerCfg.Models {
			modelName := m.Name
			if modelName == "" {
				modelName = m.ID
			}
			b.WriteString(fmt.Sprintf("- %s (%s)\n", m.ID, modelName))
		}
	}
	return b.String()
}
