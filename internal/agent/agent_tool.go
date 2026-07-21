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
	Prompt          string `json:"prompt" description:"The task for the agent to perform"`
	Model           string `json:"model,omitempty" description:"Optional. The ID of the model to run this task on, chosen from the list of available models in this tool's description. Omit to use the default model. Prefer a more capable model for hard, reasoning-heavy tasks and a smaller, faster model for routine or well-scoped tasks."`
	Mode            string `json:"mode,omitempty" description:"Optional. \"read\" (default) launches a read-only agent that can only search and read files. \"write\" launches an agent that can also edit files and run shell commands to carry out the task. Use \"write\" only when the task requires making changes."`
	ResumeSessionID string `json:"resume_session_id,omitempty" description:"Optional. Continue a previous sub-agent session instead of starting a new one -- e.g. after a failed or interrupted call, using the session ID from its error message. The prompt is delivered as a follow-up in that session, with its full prior history and progress intact. Omit to start a fresh sub-agent."`
	Background      bool   `json:"background,omitempty" description:"Optional. If true, start the sub-agent and return immediately instead of waiting for it to finish. Its result is delivered later as a follow-up message in this conversation; check on it any time with AgentList or AgentProgress(session_id)."`
}

const (
	AgentToolName = "agent"

	taskModeRead  = "read"
	taskModeWrite = "write"
)

// taskAgentKey is the registry key for a task agent, uniquely identifying
// the mode + provider + model it runs on so read/write variants and two
// providers exposing the same model ID do not collide.
func taskAgentKey(mode, providerID, modelID string) string {
	return mode + "/" + providerID + "/" + modelID
}

// normalizeTaskMode validates and defaults the tool's "mode" parameter.
func normalizeTaskMode(mode string) (string, error) {
	switch mode {
	case "", taskModeRead:
		return taskModeRead, nil
	case taskModeWrite:
		return taskModeWrite, nil
	default:
		return "", fmt.Errorf("unknown mode %q; use %q or %q", mode, taskModeRead, taskModeWrite)
	}
}

// taskAgentConfig returns the agent configuration backing a task mode: the
// read-only Task agent or the writable Task agent.
func (c *coordinator) taskAgentConfig(mode string) (config.Agent, bool) {
	id := config.AgentTask
	if mode == taskModeWrite {
		id = config.AgentTaskWrite
	}
	cfg, ok := c.cfg.Config().Agents[id]
	return cfg, ok
}

// taskAgentPrompt returns the system prompt for a task mode. Write agents
// carry out real work, so they use the full coder prompt; read agents use
// the terse investigate-and-report task prompt.
func (c *coordinator) taskAgentPrompt(mode string) (*prompt.Prompt, error) {
	opt := prompt.WithWorkingDir(c.cfg.WorkingDir())
	if mode == taskModeWrite {
		return coderPrompt(opt)
	}
	return taskPrompt(opt)
}

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	readCfg, ok := c.taskAgentConfig(taskModeRead)
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	taskPromptTmpl, err := c.taskAgentPrompt(taskModeRead)
	if err != nil {
		return nil, err
	}

	// Eagerly build the default read-only task agent (configured large
	// model) so the common path is pre-warmed. Writable agents and
	// non-default models are built lazily on first request.
	defaultAgent, err := c.buildAgent(ctx, taskPromptTmpl, readCfg, true)
	if err != nil {
		return nil, err
	}

	// Register the default agent under its key and prune idle, non-default
	// instances left over from a previous model configuration. Busy
	// instances are always kept so an in-flight sub-agent turn stays
	// steerable/cancelable even after the user switches models.
	largeCfg := c.cfg.Config().Models[config.SelectedModelTypeLarge]
	defaultKey := taskAgentKey(taskModeRead, largeCfg.Provider, largeCfg.Model)
	c.defaultTaskAgentKey.Set(defaultKey)
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

			mode, err := normalizeTaskMode(params.Mode)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			selected, err := c.taskAgentFor(ctx, mode, params.Model)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return c.runSubAgent(ctx, subAgentParams{
				Agent:           selected,
				SessionID:       sessionID,
				AgentMessageID:  agentMessageID,
				ToolCallID:      call.ID,
				Prompt:          params.Prompt,
				SessionTitle:    "New Agent Session",
				ResumeSessionID: params.ResumeSessionID,
				ToolName:        AgentToolName,
				Background:      params.Background,
			})
		},
	), nil
}

// taskAgentFor returns the task agent for the given mode and model ID,
// building and caching it on first use. An empty model ID uses the
// configured large model. A non-empty model must belong to an enabled
// provider; otherwise an error naming the valid model IDs is returned so
// the LLM can retry with a supported one.
func (c *coordinator) taskAgentFor(ctx context.Context, mode, modelID string) (SessionAgent, error) {
	agentCfg, ok := c.taskAgentConfig(mode)
	if !ok {
		return nil, fmt.Errorf("%s task agent not configured", mode)
	}

	var selected config.SelectedModel
	if modelID == "" {
		selected = c.cfg.Config().Models[config.SelectedModelTypeLarge]
	} else {
		var ok bool
		selected, ok = c.resolveTaskModel(modelID)
		if !ok {
			return nil, fmt.Errorf(
				"unknown model %q; choose one of the available model IDs: %s",
				modelID, strings.Join(c.availableModelIDs(), ", "),
			)
		}
	}

	key := taskAgentKey(mode, selected.Provider, selected.Model)
	if existing, ok := c.taskAgents.Get(key); ok && existing != nil {
		return existing, nil
	}

	taskPromptTmpl, err := c.taskAgentPrompt(mode)
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

// resolveDefaultSonnetModel picks a Claude Sonnet model from an enabled
// provider to use as agentic_fetch's default when the caller doesn't name
// a model explicitly. It prefers the provider already backing the
// configured large/small model slots (reusing an already-authenticated
// client) and otherwise falls back to the first enabled provider that
// exposes a Sonnet model. Returns false if no enabled provider has one.
func (c *coordinator) resolveDefaultSonnetModel() (config.SelectedModel, bool) {
	cfg := c.cfg.Config()

	isSonnet := func(id, name string) bool {
		return strings.Contains(strings.ToLower(id), "sonnet") ||
			strings.Contains(strings.ToLower(name), "sonnet")
	}

	fromProvider := func(providerCfg config.ProviderConfig) (config.SelectedModel, bool) {
		for _, m := range providerCfg.Models {
			if isSonnet(m.ID, m.Name) {
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

	for _, slot := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
		m, ok := cfg.Models[slot]
		if !ok {
			continue
		}
		providerCfg, ok := cfg.Providers.Get(m.Provider)
		if !ok {
			continue
		}
		if selected, ok := fromProvider(providerCfg); ok {
			return selected, true
		}
	}

	for _, providerCfg := range cfg.EnabledProviders() {
		if selected, ok := fromProvider(providerCfg); ok {
			return selected, true
		}
	}

	return config.SelectedModel{}, false
}

// resolveFetchModelSelection resolves the agentic_fetch tool's "model"
// parameter into a SelectedModel. An explicit ID is validated against the
// enabled providers (same resolution as the "agent" tool); an empty ID
// defaults to a Claude Sonnet model if one is available on an enabled
// provider, falling back to the configured small model otherwise.
func (c *coordinator) resolveFetchModelSelection(modelID string) (config.SelectedModel, error) {
	if modelID != "" {
		selected, ok := c.resolveTaskModel(modelID)
		if !ok {
			return config.SelectedModel{}, fmt.Errorf(
				"unknown model %q; choose one of the available model IDs: %s",
				modelID, strings.Join(c.availableModelIDs(), ", "),
			)
		}
		return selected, nil
	}

	if selected, ok := c.resolveDefaultSonnetModel(); ok {
		return selected, nil
	}

	small, ok := c.cfg.Config().Models[config.SelectedModelTypeSmall]
	if !ok {
		return config.SelectedModel{}, errSmallModelNotSelected
	}
	return small, nil
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
