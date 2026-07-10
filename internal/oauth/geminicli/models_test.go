package geminicli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefaultModels verifies the curated model list exposes the expected
// ids and context windows.
func TestDefaultModels(t *testing.T) {
	t.Parallel()

	models := DefaultModels()
	require.Len(t, models, 4)

	byID := make(map[string]int64, len(models))
	for _, m := range models {
		byID[m.ID] = m.ContextWindow
		require.True(t, m.CanReason, "model %s should reason", m.ID)
		require.True(t, m.SupportsImages, "model %s should support images", m.ID)
	}

	require.Equal(t, int64(1048576), byID["gemini-2.5-pro"])
	require.Equal(t, int64(1048576), byID["gemini-2.5-flash"])
	require.Equal(t, int64(1048576), byID["gemini-2.0-flash"])
	require.Equal(t, int64(1048576), byID["gemini-2.5-flash-lite"])
}
