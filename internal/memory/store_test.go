package memory

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/mem.db")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	s := NewStore(db)
	require.NoError(t, s.Init(context.Background()))
	return s
}

func TestRememberAndRecall(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"

	for _, c := range []string{
		"The test command is `task test`; run a single test with go test -run.",
		"CGO is disabled (CGO_ENABLED=0); the build is a pure-Go static binary.",
		"The user prefers concise commit messages with no body unless necessary.",
	} {
		_, created, err := s.Remember(ctx, RememberParams{Scope: scope, Content: c, Importance: 0.7})
		require.NoError(t, err)
		require.True(t, created)
	}

	hits, err := s.Recall(ctx, []string{scope}, "how do I run the tests", 5)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Contains(t, hits[0].Content, "task test", "most relevant memory should be the test-command one")

	hits, err = s.Recall(ctx, []string{scope}, "cgo build static binary", 5)
	require.NoError(t, err)
	require.Contains(t, hits[0].Content, "CGO")
}

// TestRecallRelevanceFloor reproduces the "unrelated memory injected because it
// shared a stray keyword" problem: a high-importance, strongly-worded but
// off-topic memory that only brushes the query on a single common token must be
// filtered out by the relevance floor, while a genuinely on-topic memory is
// still recalled.
func TestRecallRelevanceFloor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"

	// Off-topic but high-importance. Shares only the stray token "config" with
	// the query below.
	_, _, err := s.Remember(ctx, RememberParams{
		Scope:      scope,
		Content:    "The Hyprland cursor config keeps use_cpu_buffer true so the mirror texture never freezes on this NVIDIA machine.",
		Importance: 0.9,
	})
	require.NoError(t, err)

	// On-topic memory the query is actually about.
	_, _, err = s.Remember(ctx, RememberParams{
		Scope:      scope,
		Content:    "The RAID array descriptor stores the stripe size at byte offset forty in the config packet.",
		Importance: 0.5,
	})
	require.NoError(t, err)

	hits, err := s.Recall(ctx, []string{scope}, "raid array descriptor stripe size byte offset packet config", 6)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	for _, h := range hits {
		require.NotContains(t, h.Content, "Hyprland",
			"off-topic memory brushing the query on one token must not clear the floor")
	}
	require.Contains(t, hits[0].Content, "RAID")
}

func TestRememberDedupes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"

	_, created, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "The test command is task test", Importance: 0.5})
	require.NoError(t, err)
	require.True(t, created)

	// Near-identical restatement should merge, not add, and keep higher importance.
	m, created, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "the test command is task test", Importance: 0.9})
	require.NoError(t, err)
	require.False(t, created, "restatement must merge")
	require.Equal(t, 0.9, m.Importance)

	all, err := s.List(ctx, scope, 100)
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestScopeIsolation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	_, _, err := s.Remember(ctx, RememberParams{Scope: "proj_a", Content: "alpha uses webpack for bundling", Importance: 0.6})
	require.NoError(t, err)
	_, _, err = s.Remember(ctx, RememberParams{Scope: "proj_b", Content: "beta uses vite for bundling", Importance: 0.6})
	require.NoError(t, err)

	hits, err := s.Recall(ctx, []string{"proj_a"}, "bundling tool", 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Contains(t, hits[0].Content, "webpack")

	// Global + project scope both visible.
	_, _, err = s.Remember(ctx, RememberParams{Scope: ScopeGlobal, Content: "user prefers vim keybindings everywhere", Importance: 0.6})
	require.NoError(t, err)
	hits, err = s.Recall(ctx, []string{"proj_a", ScopeGlobal}, "keybindings preference", 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Contains(t, hits[0].Content, "vim")
}

func TestForget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"
	m, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "the deploy script lives in scripts/deploy.sh", Importance: 0.6})
	require.NoError(t, err)

	n, err := s.Forget(ctx, scope, m.ID)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	all, err := s.List(ctx, scope, 10)
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestForgetByQuery(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"
	_, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "the linter is golangci-lint run via task lint", Importance: 0.6})
	require.NoError(t, err)

	n, err := s.Forget(ctx, scope, "golangci-lint linter")
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)
}

func TestReinforceRelevance(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"

	m, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "the deploy script lives in scripts/deploy.sh", Importance: 0.5})
	require.NoError(t, err)

	require.NoError(t, s.ReinforceRelevance(ctx, m.ID, true))
	all, err := s.List(ctx, scope, 10)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.InDelta(t, 0.5+relevanceConfirmDelta, all[0].Importance, 1e-9)

	require.NoError(t, s.ReinforceRelevance(ctx, m.ID, false))
	all, err = s.List(ctx, scope, 10)
	require.NoError(t, err)
	require.InDelta(t, 0.5+relevanceConfirmDelta+relevanceRejectDelta, all[0].Importance, 1e-9)
}

func TestReinforceRelevanceClampsToBounds(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"

	low, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "a low-importance fact about widgets", Importance: 0.02})
	require.NoError(t, err)
	require.NoError(t, s.ReinforceRelevance(ctx, low.ID, false))
	all, err := s.List(ctx, scope, 10)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, 0.0, all[0].Importance, "importance must clamp at 0, never go negative")

	high, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "a high-importance fact about gadgets", Importance: 0.99})
	require.NoError(t, err)
	require.NoError(t, s.ReinforceRelevance(ctx, high.ID, true))
	all, err = s.List(ctx, scope, 10)
	require.NoError(t, err)
	require.Len(t, all, 2)
	for _, m := range all {
		if m.ID == high.ID {
			require.Equal(t, 1.0, m.Importance, "importance must clamp at 1, never exceed it")
		}
	}
}

// judgeState is a test helper returning a memory's current judge_interval
// and recalls_since_judge, so tests can assert on the backoff bookkeeping
// directly instead of only inferring it indirectly through BumpJudgeCounter.
func judgeState(t *testing.T, s *Store, id string) (interval, recalls int) {
	t.Helper()
	err := s.db.QueryRow(`SELECT judge_interval, recalls_since_judge FROM memories WHERE id=?`, id).Scan(&interval, &recalls)
	require.NoError(t, err)
	return interval, recalls
}

func TestBumpJudgeCounter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"

	m, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "the deploy script lives in scripts/deploy.sh", Importance: 0.5})
	require.NoError(t, err)

	// judge_interval starts at 1, so the very first bump is immediately due.
	due, err := s.BumpJudgeCounter(ctx, m.ID)
	require.NoError(t, err)
	require.True(t, due, "a fresh memory's first recall must be due for judgment")

	interval, recalls := judgeState(t, s, m.ID)
	require.Equal(t, 1, interval)
	require.Equal(t, 1, recalls)
}

func TestBumpJudgeCounterMissingMemoryIsNotDue(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	due, err := s.BumpJudgeCounter(context.Background(), "does-not-exist")
	require.NoError(t, err, "a missing/superseded memory must not error, just report not-due")
	require.False(t, due)
}

func TestReinforceRelevanceBackoffSchedule(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"

	m, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "the deploy script lives in scripts/deploy.sh", Importance: 0.5})
	require.NoError(t, err)

	// First recall is due (interval starts at 1); confirm it.
	due, err := s.BumpJudgeCounter(ctx, m.ID)
	require.NoError(t, err)
	require.True(t, due)
	require.NoError(t, s.ReinforceRelevance(ctx, m.ID, true))

	interval, recalls := judgeState(t, s, m.ID)
	require.Equal(t, 2, interval, "a confirm must double the interval")
	require.Equal(t, 0, recalls, "a judgment must reset the recall counter")

	// Next recall (1 of 2 needed) must NOT be due yet.
	due, err = s.BumpJudgeCounter(ctx, m.ID)
	require.NoError(t, err)
	require.False(t, due, "must not be due again until the doubled interval is reached")

	// Second recall (2 of 2) reaches the interval: due again.
	due, err = s.BumpJudgeCounter(ctx, m.ID)
	require.NoError(t, err)
	require.True(t, due)

	// Reject this time: interval must reset to the minimum, not keep growing.
	require.NoError(t, s.ReinforceRelevance(ctx, m.ID, false))
	interval, recalls = judgeState(t, s, m.ID)
	require.Equal(t, judgeIntervalMin, interval, "a reject must reset the interval to the minimum for a prompt re-test")
	require.Equal(t, 0, recalls)
}

func TestReinforceRelevanceIntervalCapsAtMax(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"

	m, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: "the deploy script lives in scripts/deploy.sh", Importance: 0.5})
	require.NoError(t, err)

	// Repeatedly confirm past the point where doubling would exceed judgeIntervalMax.
	for range 10 {
		_, err := s.BumpJudgeCounter(ctx, m.ID)
		require.NoError(t, err)
		require.NoError(t, s.ReinforceRelevance(ctx, m.ID, true))
	}

	interval, _ := judgeState(t, s, m.ID)
	require.Equal(t, judgeIntervalMax, interval, "interval must cap at judgeIntervalMax, never grow unbounded")
}

func TestEviction(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	scope := "proj_x"
	// Insert 10 distinct memories with a cap of 5; keep only the 5 highest weight.
	for i, c := range []string{
		"fact one about the alpha module and its config",
		"fact two about the beta service and its ports",
		"fact three about the gamma database schema",
		"fact four about the delta queue and workers",
		"fact five about the epsilon cache layer",
		"fact six about the zeta auth flow",
		"fact seven about the eta logging setup",
		"fact eight about the theta metrics pipeline",
		"fact nine about the iota feature flags",
		"fact ten about the kappa deployment steps",
	} {
		imp := 0.9
		if i < 5 {
			imp = 0.1 // low-importance early ones should be evicted first
		}
		_, _, err := s.Remember(ctx, RememberParams{Scope: scope, Content: c, Importance: imp, MaxPerScope: 5})
		require.NoError(t, err)
	}
	all, err := s.List(ctx, scope, 100)
	require.NoError(t, err)
	require.Len(t, all, 5)
	for _, m := range all {
		require.GreaterOrEqual(t, m.Importance, 0.5, "low-importance memories should have been evicted")
	}
}

func TestRecallEmptyQuery(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	hits, err := s.Recall(context.Background(), []string{"proj_x"}, "!!! ??? ...", 5)
	require.NoError(t, err)
	require.Empty(t, hits)
}

func TestProjectScopeStable(t *testing.T) {
	t.Parallel()
	require.Equal(t, ProjectScope("/tmp/foo"), ProjectScope("/tmp/foo/"))
	require.NotEqual(t, ProjectScope("/tmp/foo"), ProjectScope("/tmp/bar"))
}
