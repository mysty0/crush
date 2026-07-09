package agent

import (
	"context"
	_ "embed"
	"os"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/config"
)

//go:embed templates/coder.md.tpl
var coderPromptTmpl []byte

//go:embed templates/task.md.tpl
var taskPromptTmpl []byte

//go:embed templates/initialize.md.tpl
var initializePromptTmpl []byte

func coderPrompt(opts ...prompt.Option) (*prompt.Prompt, error) {
	tmpl := coderPromptTmpl
	// CRUSH_CODER_PROMPT_FILE swaps the coder system prompt for an external
	// template file. Used to A/B test alternate prompts without a rebuild.
	if path := os.Getenv("CRUSH_CODER_PROMPT_FILE"); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			tmpl = data
		}
	}
	systemPrompt, err := prompt.NewPrompt("coder", string(tmpl), opts...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

func taskPrompt(opts ...prompt.Option) (*prompt.Prompt, error) {
	systemPrompt, err := prompt.NewPrompt("task", string(taskPromptTmpl), opts...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

func InitializePrompt(cfg *config.ConfigStore) (string, error) {
	systemPrompt, err := prompt.NewPrompt("initialize", string(initializePromptTmpl))
	if err != nil {
		return "", err
	}
	return systemPrompt.Build(context.Background(), "", "", cfg)
}
