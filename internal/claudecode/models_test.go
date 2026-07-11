package claudecode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelsPopulatesReasoningLevelsFromEffortCapabilities is a
// regression test for reasoning-effort levels (including newer ones
// like xhigh/max) never showing up in the picker: Models() must read
// each model's actual capabilities.effort block from the live
// /v1/models response and populate ReasoningLevels/DefaultReasoningEffort
// accordingly, rather than leaving them unset and falling back to the
// generic off/low/medium/high thinking-budget picker for every model.
func TestModelsPopulatesReasoningLevelsFromEffortCapabilities(t *testing.T) {
	writeTestCredentials(t, "test-access-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "claude-opus-4-7",
					"display_name": "Claude Opus 4.7",
					"max_input_tokens": 200000,
					"max_tokens": 64000,
					"capabilities": {
						"thinking": {"supported": false},
						"image_input": {"supported": true},
						"effort": {
							"supported": true,
							"low": {"supported": true},
							"medium": {"supported": true},
							"high": {"supported": true},
							"xhigh": {"supported": true},
							"max": {"supported": true}
						}
					}
				},
				{
					"id": "claude-opus-4-5-20251101",
					"display_name": "Claude Opus 4.5",
					"max_input_tokens": 200000,
					"max_tokens": 64000,
					"capabilities": {
						"thinking": {"supported": true},
						"image_input": {"supported": true},
						"effort": {
							"supported": true,
							"low": {"supported": true},
							"medium": {"supported": true},
							"high": {"supported": true},
							"xhigh": {"supported": false},
							"max": {"supported": false}
						}
					}
				},
				{
					"id": "claude-3-5-haiku-20241022",
					"display_name": "Claude Haiku 3.5",
					"max_input_tokens": 200000,
					"max_tokens": 8192,
					"capabilities": {
						"thinking": {"supported": false},
						"image_input": {"supported": true},
						"effort": {"supported": false}
					}
				}
			]
		}`))
	}))
	defer srv.Close()

	s := NewSource()
	s.baseURL = srv.URL

	models, err := s.Models(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 3)

	byID := make(map[string]int)
	for i, m := range models {
		byID[m.ID] = i
	}

	opus47 := models[byID["claude-opus-4-7"]]
	assert.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, opus47.ReasoningLevels)
	assert.Equal(t, "high", opus47.DefaultReasoningEffort)
	assert.True(t, opus47.CanReason, "effort support alone should count as reasoning capable")

	opus45 := models[byID["claude-opus-4-5-20251101"]]
	assert.Equal(t, []string{"low", "medium", "high"}, opus45.ReasoningLevels, "xhigh/max must not be offered when the model doesn't support them")
	assert.Equal(t, "high", opus45.DefaultReasoningEffort)
	assert.True(t, opus45.CanReason)

	haiku := models[byID["claude-3-5-haiku-20241022"]]
	assert.Empty(t, haiku.ReasoningLevels)
	assert.Empty(t, haiku.DefaultReasoningEffort)
	assert.False(t, haiku.CanReason)
}

// TestModelsUsesSourceBaseURLOverride is a regression test for Models()
// hardcoding the package-level BaseURL constant instead of honoring a
// Source's baseURL override, which made it impossible to point model
// discovery (unlike Usage) at a test server or an alternate endpoint.
func TestModelsUsesSourceBaseURLOverride(t *testing.T) {
	writeTestCredentials(t, "test-access-token")

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()

	s := NewSource()
	s.baseURL = srv.URL

	_, err := s.Models(context.Background())
	require.NoError(t, err)
	assert.True(t, called, "Models() should have hit the Source's overridden baseURL, not the hardcoded default")
}

func TestEffortLevels(t *testing.T) {
	t.Parallel()

	assert.Nil(t, effortLevels(effortCapabilities{Supported: false}))

	c := effortCapabilities{Supported: true}
	c.Low.Supported = true
	c.High.Supported = true
	c.Max.Supported = true
	assert.Equal(t, []string{"low", "high", "max"}, effortLevels(c))
}

func TestDefaultEffortLevel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "", defaultEffortLevel(nil))
	assert.Equal(t, "high", defaultEffortLevel([]string{"low", "medium", "high", "xhigh", "max"}))
	assert.Equal(t, "high", defaultEffortLevel([]string{"low", "medium", "high"}))
	assert.Equal(t, "medium", defaultEffortLevel([]string{"low", "medium"}), "falls back to the highest supported level when high isn't offered")
}
