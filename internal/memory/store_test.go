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
