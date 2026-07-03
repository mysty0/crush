package tools

import (
	"context"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"charm.land/fantasy"
)

//go:embed web_search.md.tpl
var webSearchDescriptionTmpl []byte

var webSearchDescriptionTpl = template.Must(
	template.New("webSearchDescription").
		Parse(string(webSearchDescriptionTmpl)),
)

// NativeSearchFunc performs a web search using the active model provider's
// built-in web search and returns a formatted result string. It is supplied
// by the coordinator (which owns the model) when the "native" provider is
// selected. A nil return with a nil error means the active provider has no
// native web search and the caller should fall back to DuckDuckGo.
type NativeSearchFunc func(ctx context.Context, query string, maxResults int) (string, bool, error)

// WebSearchOptions configures the WebSearch tool.
type WebSearchOptions struct {
	// UseNative selects the provider's built-in web search over DuckDuckGo.
	UseNative bool
	// DefaultMaxResults is used when the model does not specify max_results.
	DefaultMaxResults int
	// Native performs a native provider web search. Required when UseNative
	// is true; ignored otherwise.
	Native NativeSearchFunc
}

// NewWebSearchTool creates a web search tool for sub-agents (no permissions needed).
func NewWebSearchTool(client *http.Client, opts WebSearchOptions) fantasy.AgentTool {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 10
		transport.IdleConnTimeout = 90 * time.Second

		client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	}

	return fantasy.NewParallelAgentTool(
		WebSearchToolName,
		renderToolDescription(webSearchDescriptionTpl),
		func(ctx context.Context, params WebSearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Query == "" {
				return fantasy.NewTextErrorResponse("query is required"), nil
			}

			maxResults := params.MaxResults
			if maxResults <= 0 {
				maxResults = opts.DefaultMaxResults
			}
			if maxResults <= 0 {
				maxResults = 10
			}
			if maxResults > 20 {
				maxResults = 20
			}

			// Native provider search rides the model provider's
			// authenticated request (covered by a subscription where
			// applicable). Fall back to DuckDuckGo when unavailable.
			if opts.UseNative && opts.Native != nil {
				out, handled, err := opts.Native(ctx, params.Query, maxResults)
				if err != nil {
					return fantasy.NewTextErrorResponse("Failed to search: " + err.Error()), nil
				}
				if handled {
					slog.Debug("Web search completed (native)", "query", params.Query)
					return fantasy.NewTextResponse(out), nil
				}
				slog.Debug("Native web search unavailable, falling back to DuckDuckGo", "query", params.Query)
			}

			maybeDelaySearch()
			results, err := searchDuckDuckGo(ctx, client, params.Query, maxResults)
			slog.Debug("Web search completed", "query", params.Query, "results", len(results), "err", err)
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to search: " + err.Error()), nil
			}

			return fantasy.NewTextResponse(formatSearchResults(results)), nil
		},
	)
}
