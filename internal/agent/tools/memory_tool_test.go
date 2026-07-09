package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/memory"
	"github.com/stretchr/testify/require"
)

func newMemStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/mem.db")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	s := memory.NewStore(db)
	require.NoError(t, s.Init(context.Background()))
	return s
}

func runTool(t *testing.T, tool fantasy.AgentTool, name string, params any) fantasy.ToolResponse {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Name: name, Input: string(raw)})
	require.NoError(t, err)
	return resp
}

func TestRememberRecallForgetTools(t *testing.T) {
	t.Parallel()
	store := newMemStore(t)
	dir := t.TempDir()

	remember := NewRememberTool(store, dir, 500)
	recall := NewRecallTool(store, dir)
	forget := NewForgetTool(store, dir)

	// Remember a fact.
	resp := runTool(t, remember, RememberToolName, RememberParams{
		Content: "The test command for this project is `task test`.", Importance: 0.8,
	})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "Remembered")

	// Recall it.
	resp = runTool(t, recall, RecallToolName, RecallParams{Query: "how do I run the tests"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "task test")

	// Forget it.
	resp = runTool(t, forget, ForgetToolName, ForgetParams{Target: "task test command"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "Forgot")

	// Gone.
	resp = runTool(t, recall, RecallToolName, RecallParams{Query: "test command"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No relevant memories")
}

func TestRememberGlobalScope(t *testing.T) {
	t.Parallel()
	store := newMemStore(t)
	dir := t.TempDir()
	remember := NewRememberTool(store, dir, 500)
	recall := NewRecallTool(store, dir)

	resp := runTool(t, remember, RememberToolName, RememberParams{
		Content: "The user prefers tabs over spaces.", Scope: "global", Kind: "preference",
	})
	require.False(t, resp.IsError, resp.Content)

	// Recall searches both project and global.
	resp = runTool(t, recall, RecallToolName, RecallParams{Query: "tabs spaces preference"})
	require.Contains(t, resp.Content, "tabs")
}

func TestRememberEmptyContent(t *testing.T) {
	t.Parallel()
	store := newMemStore(t)
	resp := runTool(t, NewRememberTool(store, t.TempDir(), 500), RememberToolName, RememberParams{Content: "  "})
	require.True(t, resp.IsError)
}
