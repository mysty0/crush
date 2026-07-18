package agent

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderMemoryBlock(t *testing.T) {
	t.Parallel()
	hits := []memory.Hit{
		{Memory: memory.Memory{Content: "The test command is `task test`."}},
		{Memory: memory.Memory{Content: "CGO is disabled."}},
	}
	out := renderMemoryBlock(hits)
	require.True(t, strings.HasPrefix(out, "<memory>"))
	require.True(t, strings.HasSuffix(out, "</memory>"))
	require.Contains(t, out, "- The test command is `task test`.")
	require.Contains(t, out, "- CGO is disabled.")
	require.Contains(t, out, "current code", "block must frame memories as advisory")
	require.Contains(t, out, "without commenting on it", "block must tell the model to weigh irrelevant memories silently")
}

// TestBuildMemoryRecall exercises the injection closure against a real store,
// covering the recall-and-render path and the sub-agent / empty-query gates.
// Model{} (a nil fantasy.LanguageModel) is passed for the small model so the
// background relevance judge never fires -- these tests only exercise the
// synchronous recall path.
func TestBuildMemoryRecall(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/mem.db")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := memory.NewStore(db)
	require.NoError(t, store.Init(context.Background()))

	dir := t.TempDir()
	scope := memory.ProjectScope(dir)
	_, _, err = store.Remember(context.Background(), memory.RememberParams{
		Scope: scope, Content: "The build uses CGO_ENABLED=0 for a static binary.", Importance: 0.8,
	})
	require.NoError(t, err)

	cfg, err := config.Init(dir, "", false)
	require.NoError(t, err)
	c := &coordinator{memory: store, cfg: cfg}

	recall, resetShown := c.buildMemoryRecall(false, Model{})
	require.NotNil(t, recall)
	require.NotNil(t, resetShown)
	block := recall(context.Background(), "session-1", "how is the binary built")
	require.Contains(t, block, "CGO_ENABLED=0")

	// No match → empty (no injection).
	require.Empty(t, recall(context.Background(), "session-1", "unrelated kubernetes helm chart"))

	// Sub-agents get no memory injection.
	noRecall, noReset := c.buildMemoryRecall(true, Model{})
	require.Nil(t, noRecall)
	require.Nil(t, noReset)
}

// TestBuildMemoryRecallDedupesWithinSession verifies a memory already
// injected earlier in a session is not repeated on every subsequent turn
// (it stays visible from the earlier injection still in context), but a
// different session sees it fresh, and a reset (post-summarize) allows the
// same session to see it again.
func TestBuildMemoryRecallDedupesWithinSession(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/mem.db")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := memory.NewStore(db)
	require.NoError(t, store.Init(context.Background()))

	dir := t.TempDir()
	scope := memory.ProjectScope(dir)
	_, _, err = store.Remember(context.Background(), memory.RememberParams{
		Scope: scope, Content: "The build uses CGO_ENABLED=0 for a static binary.", Importance: 0.8,
	})
	require.NoError(t, err)

	cfg, err := config.Init(dir, "", false)
	require.NoError(t, err)
	c := &coordinator{memory: store, cfg: cfg}

	recall, resetShown := c.buildMemoryRecall(false, Model{})
	require.NotNil(t, recall)

	// First turn of session-1 sees it.
	require.Contains(t, recall(context.Background(), "session-1", "how is the binary built"), "CGO_ENABLED=0")
	// Second turn of the same session, same query: already shown, suppressed.
	require.Empty(t, recall(context.Background(), "session-1", "how is the binary built"))

	// A different session sees it fresh.
	require.Contains(t, recall(context.Background(), "session-2", "how is the binary built"), "CGO_ENABLED=0")

	// After a reset (simulating a post-summarize re-establish), session-1
	// can see it again.
	resetShown("session-1")
	require.Contains(t, recall(context.Background(), "session-1", "how is the binary built"), "CGO_ENABLED=0")
}

// TestBuildMemoryRecallSkipsJudgeWithoutModel verifies that recall never
// blocks or panics when no small model is configured (Model{}, a nil
// fantasy.LanguageModel): the judge must not be spawned in that case, and
// the synchronous recall path must still return normally.
func TestBuildMemoryRecallSkipsJudgeWithoutModel(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/mem.db")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := memory.NewStore(db)
	require.NoError(t, store.Init(context.Background()))

	dir := t.TempDir()
	scope := memory.ProjectScope(dir)
	_, _, err = store.Remember(context.Background(), memory.RememberParams{
		Scope: scope, Content: "The build uses CGO_ENABLED=0 for a static binary.", Importance: 0.8,
	})
	require.NoError(t, err)

	cfg, err := config.Init(dir, "", false)
	require.NoError(t, err)
	c := &coordinator{memory: store, cfg: cfg}

	recall, _ := c.buildMemoryRecall(false, Model{})
	require.NotEmpty(t, recall(context.Background(), "session-1", "how is the binary built"))
}

// TestDueForJudge exercises the backoff-and-claim logic directly against a
// real store: a fresh memory's first recall is due, a second concurrent
// claim on the same memory is suppressed by the in-flight guard, and a
// memory not yet past its backoff interval is skipped.
func TestDueForJudge(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/mem.db")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	store := memory.NewStore(db)
	require.NoError(t, store.Init(context.Background()))

	scope := "proj_x"
	m1, _, err := store.Remember(context.Background(), memory.RememberParams{
		Scope: scope, Content: "fact one about the build", Importance: 0.5,
	})
	require.NoError(t, err)
	m2, _, err := store.Remember(context.Background(), memory.RememberParams{
		Scope: scope, Content: "fact two about the deploy", Importance: 0.5,
	})
	require.NoError(t, err)

	judging := csync.NewMap[string, struct{}]()

	// Both memories start with judge_interval=1, so their first recall is due.
	due := dueForJudge(context.Background(), store, judging, []memory.Hit{
		{Memory: m1}, {Memory: m2},
	})
	require.Len(t, due, 2)

	// A second, concurrent claim on m1 (still "in flight" from the call
	// above, since nothing released it yet) must be suppressed.
	due = dueForJudge(context.Background(), store, judging, []memory.Hit{{Memory: m1}})
	require.Empty(t, due, "an in-flight memory must not be claimed a second time")

	// Release m1's claim (simulating judgeMemoryRelevance's deferred
	// cleanup) and confirm it via ReinforceRelevance, which resets its
	// recall counter and doubles its interval to 2.
	judging.Del(m1.ID)
	require.NoError(t, store.ReinforceRelevance(context.Background(), m1.ID, true))

	// m1's next recall (1 of 2 needed) must not be due yet.
	due = dueForJudge(context.Background(), store, judging, []memory.Hit{{Memory: m1}})
	require.Empty(t, due, "must not be due again until its doubled interval is reached")
}

func TestParseRelevanceVerdict(t *testing.T) {
	t.Parallel()

	t.Run("comma-separated indices mark only those relevant", func(t *testing.T) {
		t.Parallel()
		got := parseRelevanceVerdict("1,3", 3)
		require.NotNil(t, got)
		assert.Equal(t, []bool{true, false, true}, got)
	})

	t.Run("none marks everything irrelevant", func(t *testing.T) {
		t.Parallel()
		got := parseRelevanceVerdict("none", 2)
		require.NotNil(t, got)
		assert.Equal(t, []bool{false, false}, got)
	})

	t.Run("none is case-insensitive and tolerates whitespace", func(t *testing.T) {
		t.Parallel()
		got := parseRelevanceVerdict("  None \n", 2)
		require.NotNil(t, got)
		assert.Equal(t, []bool{false, false}, got)
	})

	t.Run("out-of-range and non-numeric tokens are ignored, valid ones still count", func(t *testing.T) {
		t.Parallel()
		got := parseRelevanceVerdict("1, banana, 99, 2", 2)
		require.NotNil(t, got)
		assert.Equal(t, []bool{true, true}, got)
	})

	t.Run("empty reply is unparseable", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, parseRelevanceVerdict("", 3))
		assert.Nil(t, parseRelevanceVerdict("   ", 3))
	})

	t.Run("pure garbage with no valid tokens is unparseable", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, parseRelevanceVerdict("I cannot determine this.", 3))
	})
}
