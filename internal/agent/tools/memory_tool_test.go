package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/permission"
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
	// Remember/Forget require a session ID in context (they go through
	// the permission service, like any other mutating tool); Recall
	// ignores it. Setting it unconditionally keeps this helper shared.
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: name, Input: string(raw)})
	require.NoError(t, err)
	return resp
}

func TestRememberRecallForgetTools(t *testing.T) {
	t.Parallel()
	store := newMemStore(t)
	dir := t.TempDir()

	remember := NewRememberTool(store, &mockPermissionService{}, dir, 500)
	recall := NewRecallTool(store, dir)
	forget := NewForgetTool(store, &mockPermissionService{}, dir)

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
	remember := NewRememberTool(store, &mockPermissionService{}, dir, 500)
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
	resp := runTool(t, NewRememberTool(store, &mockPermissionService{}, t.TempDir(), 500), RememberToolName, RememberParams{Content: "  "})
	require.True(t, resp.IsError)
}

// memoryPermissionRecorder wraps mockPermissionService to capture what a
// tool actually asked permission for, and to let a test deny the request.
type memoryPermissionRecorder struct {
	mockPermissionService
	deny     bool
	requests []permission.CreatePermissionRequest
}

func (m *memoryPermissionRecorder) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	m.requests = append(m.requests, req)
	if m.deny {
		return false, nil
	}
	return true, nil
}

// TestRememberRequiresPermission is a regression test for the missing
// permission gate: Remember/Forget persist across every future session
// and get auto-injected into future turns, so unlike Recall (read-only)
// they must go through the same approval flow as any other mutating
// tool -- and be deniable.
func TestRememberRequiresPermission(t *testing.T) {
	t.Parallel()
	store := newMemStore(t)
	dir := t.TempDir()
	perms := &memoryPermissionRecorder{deny: true}

	remember := NewRememberTool(store, perms, dir, 500)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	raw, err := json.Marshal(RememberParams{Content: "The user's API key is sk-abc123."})
	require.NoError(t, err)
	resp, err := remember.Run(ctx, fantasy.ToolCall{ID: "c1", Name: RememberToolName, Input: string(raw)})
	require.NoError(t, err)

	require.True(t, resp.IsError, "a denied permission request must not silently succeed")
	require.Len(t, perms.requests, 1, "Remember must ask permission before storing")
	require.Equal(t, RememberToolName, perms.requests[0].ToolName)

	// The denied content must not actually have been persisted.
	hits, err := store.Recall(context.Background(), []string{memory.ProjectScope(dir)}, "API key", 8)
	require.NoError(t, err)
	require.Empty(t, hits, "content must not be stored when permission is denied")
}

// TestForgetRequiresPermission mirrors TestRememberRequiresPermission for
// the deletion path.
func TestForgetRequiresPermission(t *testing.T) {
	t.Parallel()
	store := newMemStore(t)
	dir := t.TempDir()

	// Seed a memory to attempt to forget.
	remember := NewRememberTool(store, &mockPermissionService{}, dir, 500)
	runTool(t, remember, RememberToolName, RememberParams{Content: "The build command is `make build`."})

	perms := &memoryPermissionRecorder{deny: true}
	forget := NewForgetTool(store, perms, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	raw, err := json.Marshal(ForgetParams{Target: "build command"})
	require.NoError(t, err)
	resp, err := forget.Run(ctx, fantasy.ToolCall{ID: "c1", Name: ForgetToolName, Input: string(raw)})
	require.NoError(t, err)

	require.True(t, resp.IsError)
	require.Len(t, perms.requests, 1)
	require.Equal(t, ForgetToolName, perms.requests[0].ToolName)

	// Still there -- the denied forget must not have deleted it.
	hits, err := store.Recall(context.Background(), []string{memory.ProjectScope(dir)}, "build command", 8)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "memory must survive a denied Forget")
}

// TestRecallDoesNotRequirePermission confirms Recall intentionally has no
// permission gate: it's read-only, matching Grep/Read, and requiring
// approval for every automatic/explicit recall would make the feature
// unusable.
func TestRecallDoesNotRequirePermission(t *testing.T) {
	t.Parallel()
	store := newMemStore(t)
	dir := t.TempDir()
	runTool(t, NewRememberTool(store, &mockPermissionService{}, dir, 500), RememberToolName,
		RememberParams{Content: "The test command for this project is `task test`."})

	recall := NewRecallTool(store, dir)
	// No session ID in context at all -- Recall must not need one.
	resp, err := recall.Run(context.Background(), fantasy.ToolCall{
		ID: "c1", Name: RecallToolName,
		Input: `{"query":"test command"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "task test")
}
