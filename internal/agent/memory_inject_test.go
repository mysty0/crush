package agent

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
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
}

// TestBuildMemoryRecall exercises the injection closure against a real store,
// covering the recall-and-render path and the sub-agent / empty-query gates.
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

	recall := c.buildMemoryRecall(false)
	require.NotNil(t, recall)
	block := recall(context.Background(), "how is the binary built")
	require.Contains(t, block, "CGO_ENABLED=0")

	// No match → empty (no injection).
	require.Empty(t, recall(context.Background(), "unrelated kubernetes helm chart"))

	// Sub-agents get no memory injection.
	require.Nil(t, c.buildMemoryRecall(true))
}
