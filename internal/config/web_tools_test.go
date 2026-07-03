package config

import "testing"

func TestToolWebSearchGetProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty defaults to duckduckgo", "", WebSearchProviderDuckDuckGo},
		{"native", WebSearchProviderNative, WebSearchProviderNative},
		{"explicit duckduckgo", WebSearchProviderDuckDuckGo, WebSearchProviderDuckDuckGo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ToolWebSearch{Provider: tt.in}.GetProvider()
			if got != tt.want {
				t.Errorf("GetProvider() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolWebFetchGetMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty defaults to markdown", "", WebFetchModeMarkdown},
		{"summarize", WebFetchModeSummarize, WebFetchModeSummarize},
		{"explicit markdown", WebFetchModeMarkdown, WebFetchModeMarkdown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ToolWebFetch{Mode: tt.in}.GetMode()
			if got != tt.want {
				t.Errorf("GetMode() = %q, want %q", got, tt.want)
			}
		})
	}
}
