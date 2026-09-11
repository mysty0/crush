package agent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
)

func TestPromptTokensCountsEveryPromptBucket(t *testing.T) {
	t.Parallel()

	// These cover providers that report cache buckets disjoint from input
	// (Anthropic and the OpenAI-compatible providers), hence
	// cacheInsideInput=false. Gemini's subset convention is covered in
	// usage_cache_semantics_test.go.

	// A turn that misses the cache reports almost nothing as InputTokens and
	// the entire prefix as CacheCreationTokens. Counting only input plus
	// cache reads reported that 51k-token turn as two tokens.
	assert.Equal(t, int64(51_463), promptTokens(fantasy.Usage{
		InputTokens:         2,
		CacheCreationTokens: 51_461,
	}, false))

	assert.Equal(t, int64(51_463), promptTokens(fantasy.Usage{
		InputTokens:     2,
		CacheReadTokens: 51_461,
	}, false))

	assert.Equal(t, int64(1_500), promptTokens(fantasy.Usage{
		InputTokens:         500,
		CacheReadTokens:     600,
		CacheCreationTokens: 400,
		OutputTokens:        9_000, // completion is not prompt
	}, false))
}

func TestUpdateSessionTokenCountersRecordsCacheSplit(t *testing.T) {
	t.Parallel()

	sess := &session.Session{}
	updateSessionTokenCounters(sess, fantasy.Usage{
		InputTokens:         2,
		CacheReadTokens:     33_876,
		CacheCreationTokens: 1_200,
		OutputTokens:        4,
	}, false)

	assert.Equal(t, int64(35_078), sess.PromptTokens)
	assert.Equal(t, int64(4), sess.CompletionTokens)
	assert.Equal(t, int64(33_876), sess.CacheReadTokens)
	assert.Equal(t, int64(1_200), sess.CacheCreationTokens)
}
