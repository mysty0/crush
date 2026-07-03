package agent

import (
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestFormatNativeSearchResponse(t *testing.T) {
	t.Parallel()

	t.Run("answer with deduped sources", func(t *testing.T) {
		t.Parallel()
		resp := &fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "Tokyo has ~37M people."},
				fantasy.SourceContent{SourceType: fantasy.SourceTypeURL, URL: "https://example.com/a", Title: "A"},
				fantasy.SourceContent{SourceType: fantasy.SourceTypeURL, URL: "https://example.com/a", Title: "A dup"},
				fantasy.SourceContent{SourceType: fantasy.SourceTypeURL, URL: "https://example.com/b", Title: "B"},
			},
		}
		got := formatNativeSearchResponse(resp)
		if !strings.Contains(got, "Tokyo has ~37M people.") {
			t.Errorf("missing answer text: %q", got)
		}
		if !strings.Contains(got, "Sources:") {
			t.Errorf("missing Sources section: %q", got)
		}
		if !strings.Contains(got, "- [A](https://example.com/a)") {
			t.Errorf("missing first source: %q", got)
		}
		if !strings.Contains(got, "- [B](https://example.com/b)") {
			t.Errorf("missing second source: %q", got)
		}
		if strings.Count(got, "https://example.com/a") != 1 {
			t.Errorf("source not deduped: %q", got)
		}
	})

	t.Run("source without title falls back to url", func(t *testing.T) {
		t.Parallel()
		resp := &fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "answer"},
				fantasy.SourceContent{SourceType: fantasy.SourceTypeURL, URL: "https://example.com/x"},
			},
		}
		got := formatNativeSearchResponse(resp)
		if !strings.Contains(got, "- [https://example.com/x](https://example.com/x)") {
			t.Errorf("expected url fallback title: %q", got)
		}
	})

	t.Run("empty response", func(t *testing.T) {
		t.Parallel()
		resp := &fantasy.Response{Content: fantasy.ResponseContent{}}
		got := formatNativeSearchResponse(resp)
		if !strings.Contains(got, "No results found") {
			t.Errorf("expected no-results message: %q", got)
		}
	})
}
