package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// TestRecordBackgroundUsage verifies auxiliary model-call usage is
// persisted: a hidden per-project, per-source session gets an assistant
// message carrying a TokenUsage part, its counters accumulate, and the
// session stays out of the top-level session list.
func TestRecordBackgroundUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q, conn)
	c := &coordinator{sessions: sessions, messages: messages}

	model := Model{
		ModelCfg: config.SelectedModel{Provider: "test", Model: "small-model"},
		FlatRate: true, // Subscription: cost must be recorded as 0.
	}
	usage := fantasy.Usage{
		InputTokens:         100,
		OutputTokens:        50,
		CacheReadTokens:     1000,
		CacheCreationTokens: 10,
	}

	scope := "proj-scope"
	c.recordBackgroundUsage(ctx, model, scope, bgSourceWebSearch, "Web searches", "Searched: golang", usage)

	sid := backgroundUsageSessionID(bgSourceWebSearch, scope)
	sess, err := sessions.Get(ctx, sid)
	require.NoError(t, err)
	require.Equal(t, backgroundUsageParent, sess.ParentSessionID, "session must be hidden (non-null parent)")
	require.Equal(t, int64(1110), sess.PromptTokens, "prompt = input + cache read + cache creation")
	require.Equal(t, int64(50), sess.CompletionTokens)
	require.Zero(t, sess.Cost, "flat-rate cost must be zero")

	// It must not surface in the top-level session list.
	list, err := sessions.List(ctx)
	require.NoError(t, err)
	for _, s := range list {
		require.NotEqual(t, sid, s.ID, "background session must be hidden from List")
	}

	// The message carries the exact per-call usage.
	msgs, err := messages.List(ctx, sid)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	u := msgs[0].TokenUsagePart()
	require.NotNil(t, u, "assistant message must carry a TokenUsage part")
	require.Equal(t, message.TokenUsage{
		InputTokens:         100,
		OutputTokens:        50,
		CacheReadTokens:     1000,
		CacheCreationTokens: 10,
		Cost:                0,
	}, *u)

	// A second call reuses the session and accumulates.
	c.recordBackgroundUsage(ctx, model, scope, bgSourceWebSearch, "Web searches", "", usage)
	sess, err = sessions.Get(ctx, sid)
	require.NoError(t, err)
	require.Equal(t, int64(2220), sess.PromptTokens)
	require.Equal(t, int64(100), sess.CompletionTokens)
	msgs, err = messages.List(ctx, sid)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
}

// TestRecordBackgroundUsageSeparatesSources verifies each source keeps its
// own hidden session so different kinds of background spend stay separable.
func TestRecordBackgroundUsageSeparatesSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q, conn)
	c := &coordinator{sessions: sessions, messages: messages}

	model := Model{ModelCfg: config.SelectedModel{Provider: "test", Model: "m"}, FlatRate: true}
	usage := fantasy.Usage{InputTokens: 10, OutputTokens: 5}

	c.recordBackgroundUsage(ctx, model, "s", bgSourceWebSearch, "Web searches", "", usage)
	c.recordBackgroundUsage(ctx, model, "s", bgSourceMemoryJudge, "Memory relevance checks", "", usage)

	_, err = sessions.Get(ctx, backgroundUsageSessionID(bgSourceWebSearch, "s"))
	require.NoError(t, err)
	_, err = sessions.Get(ctx, backgroundUsageSessionID(bgSourceMemoryJudge, "s"))
	require.NoError(t, err)
}

// TestRecordBackgroundUsageSkipsZero verifies a call that reports no usage
// records nothing (no session, no message).
func TestRecordBackgroundUsageSkipsZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q, conn)
	c := &coordinator{sessions: sessions, messages: messages}

	c.recordBackgroundUsage(ctx, Model{FlatRate: true}, "s", bgSourceWebFetch, "Web page summaries", "", fantasy.Usage{})

	_, err = sessions.Get(ctx, backgroundUsageSessionID(bgSourceWebFetch, "s"))
	require.Error(t, err, "no session should be created when usage is zero")
}
