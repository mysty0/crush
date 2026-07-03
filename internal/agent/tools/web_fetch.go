package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"
)

//go:embed web_fetch.md.tpl
var webFetchDescriptionTmpl []byte

var webFetchDescriptionTpl = template.Must(
	template.New("webFetchDescription").
		Parse(string(webFetchDescriptionTmpl)),
)

// SummarizeFunc runs a prompt over fetched page content using a small model
// and returns the model's answer. It is supplied by the coordinator (which
// owns the model) when the WebFetch tool runs in summarize mode.
type SummarizeFunc func(ctx context.Context, url, content, prompt string) (string, error)

// WebFetchOptions configures the WebFetch tool.
type WebFetchOptions struct {
	// Summarize, when set, makes WebFetch run the given prompt over the
	// fetched content with a small model instead of returning raw markdown.
	Summarize SummarizeFunc
}

// NewWebFetchTool creates a simple web fetch tool for sub-agents (no permissions needed).
func NewWebFetchTool(workingDir string, client *http.Client, opts WebFetchOptions) fantasy.AgentTool {
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
		WebFetchToolName,
		renderToolDescription(webFetchDescriptionTpl),
		func(ctx context.Context, params WebFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.URL == "" {
				return fantasy.NewTextErrorResponse("url is required"), nil
			}

			content, err := FetchURLAndConvert(ctx, client, params.URL)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to fetch URL: %s", err)), nil
			}

			// Summarize mode: run the prompt over the fetched content with a
			// small model and return that answer instead of the raw page.
			if opts.Summarize != nil {
				prompt := params.Prompt
				if prompt == "" {
					prompt = "Summarize the key information on this page."
				}
				answer, err := opts.Summarize(ctx, params.URL, content, prompt)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to summarize content: %s", err)), nil
				}
				return fantasy.NewTextResponse(fmt.Sprintf("Answer from %s:\n\n%s", params.URL, answer)), nil
			}

			hasLargeContent := len(content) > LargeContentThreshold
			var result strings.Builder

			if hasLargeContent {
				tempFile, err := os.CreateTemp(workingDir, "page-*.md")
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to create temporary file: %s", err)), nil
				}
				tempFilePath := tempFile.Name()

				if _, err := tempFile.WriteString(content); err != nil {
					_ = tempFile.Close() // Best effort close
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to write content to file: %s", err)), nil
				}
				if err := tempFile.Close(); err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to close temporary file: %s", err)), nil
				}

				fmt.Fprintf(&result, "Fetched content from %s (large page)\n\n", params.URL)
				fmt.Fprintf(&result, "Content saved to: %s\n\n", tempFilePath)
				result.WriteString("Use the view and grep tools to analyze this file.")
			} else {
				fmt.Fprintf(&result, "Fetched content from %s:\n\n", params.URL)
				result.WriteString(content)
			}

			return fantasy.NewTextResponse(result.String()), nil
		},
	)
}
