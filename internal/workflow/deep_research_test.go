package workflow

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// deepResearchMockRunner drives the real deep-research.lua script
// end-to-end without any network access. It returns schema-shaped
// data by inspecting which properties the requested schema declares,
// which is enough to distinguish the workflow's five call types
// (scope, search, fetch, verify, synthesize) without hardcoding
// labels.
type deepResearchMockRunner struct {
	calls atomic.Int64

	mu     sync.Mutex
	models map[string]string // label -> requested model
}

func (m *deepResearchMockRunner) RunAgent(_ context.Context, req AgentRequest) (string, error) {
	m.calls.Add(1)
	m.mu.Lock()
	if m.models == nil {
		m.models = map[string]string{}
	}
	m.models[req.Label] = req.Model
	m.mu.Unlock()
	return "ok:" + req.Label, nil
}

func (m *deepResearchMockRunner) CoerceObject(_ context.Context, _ string, schema *Schema, _ string) (any, error) {
	has := func(name string) bool {
		_, ok := schema.Properties[name]
		return ok
	}

	switch {
	case has("angles"): // SCOPE_SCHEMA
		return map[string]any{
			"question": "test question",
			"summary":  "test decomposition",
			"angles": []any{
				map[string]any{"label": "primary", "query": "q1", "rationale": "r1"},
				map[string]any{"label": "technical", "query": "q2", "rationale": "r2"},
				map[string]any{"label": "skeptical", "query": "q3", "rationale": "r3"},
			},
		}, nil

	case has("results"): // SEARCH_SCHEMA
		return map[string]any{
			"results": []any{
				map[string]any{"url": "https://example.com/a", "title": "A", "snippet": "s", "relevance": "high"},
				map[string]any{"url": "https://example.com/b", "title": "B", "snippet": "s", "relevance": "medium"},
			},
		}, nil

	case has("claims") && has("sourceQuality"): // EXTRACT_SCHEMA
		return map[string]any{
			"sourceQuality": "primary",
			"publishDate":   "2026-01-01",
			"claims": []any{
				map[string]any{"claim": "The sky is blue.", "quote": "the sky appears blue", "importance": "central"},
			},
		}, nil

	case has("refuted"): // VERDICT_SCHEMA
		return map[string]any{
			"refuted":    false,
			"evidence":   "well-supported by primary source",
			"confidence": "high",
		}, nil

	case has("findings"): // REPORT_SCHEMA
		return map[string]any{
			"summary": "Test synthesis summary.",
			"findings": []any{
				map[string]any{
					"claim": "The sky is blue.", "confidence": "high",
					"sources": []any{"https://example.com/a"}, "evidence": "well-supported", "vote": "3-0",
				},
			},
			"caveats":       "none",
			"openQuestions": []any{"what about sunsets?"},
		}, nil

	default:
		return map[string]any{}, nil
	}
}

func TestDeepResearch_EndToEnd(t *testing.T) {
	t.Parallel()
	w, err := Find("deep-research")
	require.NoError(t, err)
	require.NotNil(t, w)

	runner := &deepResearchMockRunner{}
	var phases []string
	result, err := Run(t.Context(), RunOptions{
		Script: w.Script,
		Args:   "does the sky appear blue to observers on earth",
		Runner: runner,
		Budget: Budget{Timeout: 30 * time.Second},
		Progress: func(e ProgressEvent) {
			if e.Phase != "" {
				phases = append(phases, e.Phase)
			}
		},
	})
	require.NoError(t, err)

	report, ok := result.(map[string]any)
	require.True(t, ok, "expected a report map, got %T: %#v", result, result)
	require.NotContains(t, report, "error")
	require.Equal(t, "does the sky appear blue to observers on earth", report["question"])
	require.Equal(t, "Test synthesis summary.", report["summary"])
	require.Contains(t, report, "findings")
	require.Contains(t, report, "sources")
	require.Contains(t, report, "stats")

	// "Fetch" is announced once per search angle as its streaming
	// branch starts, so it may repeat; dedupe consecutive repeats
	// before checking the phase sequence.
	var dedup []string
	for _, p := range phases {
		if len(dedup) == 0 || dedup[len(dedup)-1] != p {
			dedup = append(dedup, p)
		}
	}
	require.Equal(t, []string{"Scope", "Search", "Fetch", "Verify", "Synthesize"}, dedup)
	require.Greater(t, runner.calls.Load(), int64(0))
	// Search calls are mechanical ranking/filtering work and run on
	// the small model; extraction/verification/synthesis quality
	// determines report trustworthiness, so they stay on the default
	// model (an empty model request).
	runner.mu.Lock()
	defer runner.mu.Unlock()
	sawSearch := false
	for label, model := range runner.models {
		if strings.HasPrefix(label, "search:") {
			sawSearch = true
			require.Equal(t, "small", model, "search call %q should request the small model", label)
		} else if label == "scope" || label == "synthesize" || strings.HasPrefix(label, "fetch:") || strings.HasPrefix(label, "v") {
			require.Empty(t, model, "call %q should use the default model", label)
		}
	}
	require.True(t, sawSearch, "expected at least one search:* call")
}

func TestDeepResearch_EmptyQuestionAsksForClarification(t *testing.T) {
	t.Parallel()
	w, err := Find("deep-research")
	require.NoError(t, err)

	result, err := Run(t.Context(), RunOptions{
		Script: w.Script,
		Args:   "   ",
		Runner: &deepResearchMockRunner{},
	})
	require.NoError(t, err)
	m := result.(map[string]any)
	require.Contains(t, m, "error")
}
