package agent

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
)

// nativeWebSearch performs a web search using the active model provider's
// built-in web search tool. It currently supports Anthropic-family providers
// (including the Claude subscription provider), where the search rides the
// same authenticated request as normal model calls.
//
// The bool return reports whether the search was handled natively. When it is
// false (nil error), the active provider has no native web search and the
// caller should fall back to DuckDuckGo.
func (c *coordinator) nativeWebSearch(ctx context.Context, query string, maxResults int) (string, bool, error) {
	large, _, err := c.buildAgentModels(ctx, false)
	if err != nil {
		return "", false, err
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(large.ModelCfg.Provider)
	if !ok {
		return "", false, nil
	}

	// Only Anthropic-family providers expose a native web search tool in the
	// current model SDK. Anything else falls back to DuckDuckGo.
	if providerCfg.Type != anthropic.Name {
		return "", false, nil
	}

	searchTool := anthropic.WebSearchTool(&anthropic.WebSearchToolOptions{
		MaxUses: int64(maxResults),
	})

	prompt := fantasy.Prompt{
		fantasy.NewUserMessage("Perform a web search for the query: " + query),
	}

	resp, err := large.Model.Generate(ctx, fantasy.Call{
		Prompt: prompt,
		Tools:  []fantasy.Tool{searchTool},
	})
	if err != nil {
		return "", false, err
	}

	c.recordBackgroundUsage(ctx, large, memory.ProjectScope(c.cfg.WorkingDir()),
		bgSourceWebSearch, "Web searches", "Searched: "+query, resp.Usage)

	return formatNativeSearchResponse(resp), true, nil
}

// formatNativeSearchResponse renders the model's answer plus a Sources section
// built from the URL sources it cited, mirroring how Claude Code presents
// web search results.
func formatNativeSearchResponse(resp *fantasy.Response) string {
	var sb strings.Builder

	answer := strings.TrimSpace(resp.Content.Text())
	if answer != "" {
		sb.WriteString(answer)
	}

	var lines []string
	seen := map[string]bool{}
	for _, src := range resp.Content.Sources() {
		if src.SourceType != fantasy.SourceTypeURL || src.URL == "" || seen[src.URL] {
			continue
		}
		seen[src.URL] = true
		title := src.Title
		if title == "" {
			title = src.URL
		}
		lines = append(lines, fmt.Sprintf("- [%s](%s)", title, src.URL))
	}

	if len(lines) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("Sources:\n")
		sb.WriteString(strings.Join(lines, "\n"))
		sb.WriteString("\n")
	}

	if sb.Len() == 0 {
		return "No results found. Try rephrasing your search."
	}
	return sb.String()
}

// webSearchOptions builds the WebSearch tool options from config, wiring the
// native searcher when the native provider is selected.
func (c *coordinator) webSearchOptions() tools.WebSearchOptions {
	cfg := c.cfg.Config().Tools.WebSearch
	opts := tools.WebSearchOptions{
		DefaultMaxResults: 10,
	}
	if cfg.MaxResults != nil && *cfg.MaxResults > 0 {
		opts.DefaultMaxResults = *cfg.MaxResults
	}
	if cfg.GetProvider() == config.WebSearchProviderNative {
		opts.UseNative = true
		opts.Native = c.nativeWebSearch
	}
	return opts
}

// summarizeWebContent runs a prompt over fetched page content with the small
// model and returns its answer.
func (c *coordinator) summarizeWebContent(ctx context.Context, url, content, userPrompt string) (string, error) {
	_, small, err := c.buildAgentModels(ctx, true)
	if err != nil {
		return "", err
	}

	msg := fmt.Sprintf(
		"Web page content:\n---\n%s\n---\n\n%s\n\nProvide a concise response based only on the content above.",
		content, userPrompt,
	)

	resp, err := small.Model.Generate(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage(msg)},
	})
	if err != nil {
		return "", err
	}

	c.recordBackgroundUsage(ctx, small, memory.ProjectScope(c.cfg.WorkingDir()),
		bgSourceWebFetch, "Web page summaries", "Summarized: "+url, resp.Usage)
	return strings.TrimSpace(resp.Content.Text()), nil
}

// webFetchOptions builds the WebFetch tool options from config, wiring the
// summarizer when summarize mode is selected.
func (c *coordinator) webFetchOptions() tools.WebFetchOptions {
	if c.cfg.Config().Tools.WebFetch.GetMode() == config.WebFetchModeSummarize {
		return tools.WebFetchOptions{Summarize: c.summarizeWebContent}
	}
	return tools.WebFetchOptions{}
}
