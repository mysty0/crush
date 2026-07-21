package codex

import (
	"context"
	"fmt"
	"iter"
	"net/http"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
)

type ProviderOptions struct {
	BaseURL    string
	APIKey     string
	Headers    map[string]string
	HTTPClient *http.Client
}

type Provider struct {
	fantasy.Provider
}

func NewProvider(options ProviderOptions) (fantasy.Provider, error) {
	baseURL := options.BaseURL
	if baseURL == "" {
		baseURL = BaseURL
	}

	opts := []openai.Option{
		openai.WithBaseURL(baseURL),
		openai.WithUseResponsesAPI(),
		openai.WithAPIKey(options.APIKey),
	}
	if options.HTTPClient != nil {
		opts = append(opts, openai.WithHTTPClient(options.HTTPClient))
	}
	if len(options.Headers) > 0 {
		opts = append(opts, openai.WithHeaders(options.Headers))
	}

	p, err := openai.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("create openai provider for codex: %w", err)
	}

	return wrapProvider(p), nil
}

func wrapProvider(p fantasy.Provider) fantasy.Provider {
	return &Provider{Provider: p}
}

func (p *Provider) LanguageModel(ctx context.Context, modelID string) (fantasy.LanguageModel, error) {
	m, err := p.Provider.LanguageModel(ctx, modelID)
	if err != nil {
		return nil, err
	}
	return &LanguageModel{LanguageModel: m}, nil
}

type LanguageModel struct {
	fantasy.LanguageModel
}

func (m *LanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	call.MaxOutputTokens = nil
	return m.LanguageModel.Generate(ctx, call)
}

func (m *LanguageModel) Stream(ctx context.Context, call fantasy.Call) (iter.Seq[fantasy.StreamPart], error) {
	call.MaxOutputTokens = nil
	return m.LanguageModel.Stream(ctx, call)
}
