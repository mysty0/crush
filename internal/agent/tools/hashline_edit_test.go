package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/hashline"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

func hashlineTestTool(store *hashline.Store, workingDir string) fantasy.AgentTool {
	perms := &mockPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	return NewHashlineEditTool(nil, perms, &mockHistoryService{}, mockFileTracker{}, store, nil, workingDir)
}

func runHashlineEdit(t *testing.T, tool fantasy.AgentTool, sessionID, input string) fantasy.ToolResponse {
	t.Helper()
	raw, err := json.Marshal(HashlineEditParams{Input: input})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sessionID)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: EditToolName, Input: string(raw)})
	require.NoError(t, err)
	return resp
}

// recordRead simulates a Read: store a snapshot and return the tag the model
// would see in the header.
func recordRead(t *testing.T, store *hashline.Store, sessionID, absPath, text string) string {
	t.Helper()
	return store.Record(sessionID, absPath, text, nil)
}

func TestHashlineEditRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	content := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	require.NoError(t, os.WriteFile(file, []byte(content), 0o644))

	store := hashline.NewStore()
	abs, _ := filepath.Abs(file)
	tag := recordRead(t, store, "sess", abs, content)

	tool := hashlineTestTool(store, dir)
	input := "[a.go#" + tag + "]\nSWAP 4.=4:\n+\tprintln(\"hello\")"
	resp := runHashlineEdit(t, tool, "sess", input)
	require.False(t, resp.IsError, resp.Content)

	got, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n", string(got))
	require.Contains(t, resp.Content, "[a.go#")
}

func TestHashlineEditStaleTagRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	require.NoError(t, os.WriteFile(file, []byte("a\nb\nc\n"), 0o644))

	store := hashline.NewStore()
	abs, _ := filepath.Abs(file)
	recordRead(t, store, "sess", abs, "a\nb\nc\n")

	tool := hashlineTestTool(store, dir)
	// A tag that does not match the live file.
	resp := runHashlineEdit(t, tool, "sess", "[a.go#0000]\nSWAP 1.=1:\n+X")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "0000")

	// File unchanged.
	got, _ := os.ReadFile(file)
	require.Equal(t, "a\nb\nc\n", string(got))
}

func TestHashlineEditMultiFileAtomicPreflight(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(fileA, []byte("a1\na2\n"), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte("b1\nb2\n"), 0o644))

	store := hashline.NewStore()
	absA, _ := filepath.Abs(fileA)
	tagA := recordRead(t, store, "sess", absA, "a1\na2\n")

	tool := hashlineTestTool(store, dir)
	// Section A is valid; section B carries a bad tag. The whole batch must
	// abort, leaving A unchanged.
	input := "[a.txt#" + tagA + "]\nSWAP 1.=1:\n+A1\n[b.txt#0000]\nSWAP 1.=1:\n+B1"
	resp := runHashlineEdit(t, tool, "sess", input)
	require.True(t, resp.IsError)

	gotA, _ := os.ReadFile(fileA)
	require.Equal(t, "a1\na2\n", string(gotA), "section A must not be written when section B fails preflight")
}

func TestHashlineEditCRLFRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "crlf.txt")
	require.NoError(t, os.WriteFile(file, []byte("a\r\nb\r\nc\r\n"), 0o644))

	store := hashline.NewStore()
	abs, _ := filepath.Abs(file)
	// Read normalizes to LF for the tag.
	tag := recordRead(t, store, "sess", abs, "a\nb\nc\n")

	tool := hashlineTestTool(store, dir)
	resp := runHashlineEdit(t, tool, "sess", "[crlf.txt#"+tag+"]\nSWAP 2.=2:\n+B")
	require.False(t, resp.IsError, resp.Content)

	got, _ := os.ReadFile(file)
	require.Equal(t, "a\r\nB\r\nc\r\n", string(got), "CRLF line endings must be preserved")
}

func TestHashlineEditFileNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := hashline.NewStore()
	tool := hashlineTestTool(store, dir)
	resp := runHashlineEdit(t, tool, "sess", "[nope.txt#0000]\nDEL 1")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "not found")
}

func TestHashlineEditRemoveFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "gone.txt")
	require.NoError(t, os.WriteFile(file, []byte("a\nb\n"), 0o644))
	store := hashline.NewStore()
	abs, _ := filepath.Abs(file)
	tag := recordRead(t, store, "sess", abs, "a\nb\n")

	tool := hashlineTestTool(store, dir)
	resp := runHashlineEdit(t, tool, "sess", "[gone.txt#"+tag+"]\nREM")
	require.False(t, resp.IsError, resp.Content)
	require.NoFileExists(t, file)
	require.Contains(t, resp.Content, "Deleted")
}

func TestHashlineEditMoveFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello\nworld\n"), 0o644))
	store := hashline.NewStore()
	abs, _ := filepath.Abs(src)
	tag := recordRead(t, store, "sess", abs, "hello\nworld\n")

	tool := hashlineTestTool(store, dir)
	// Edit then move in one section.
	resp := runHashlineEdit(t, tool, "sess", "[src.txt#"+tag+"]\nSWAP 1.=1:\n+HELLO\nMV dst.txt")
	require.False(t, resp.IsError, resp.Content)
	require.NoFileExists(t, src)
	got, err := os.ReadFile(filepath.Join(dir, "dst.txt"))
	require.NoError(t, err)
	require.Equal(t, "HELLO\nworld\n", string(got))
	require.Contains(t, resp.Content, "Moved")
}

func TestHashlineEditMoveNoEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(src, []byte("x\n"), 0o644))
	store := hashline.NewStore()
	abs, _ := filepath.Abs(src)
	tag := recordRead(t, store, "sess", abs, "x\n")

	tool := hashlineTestTool(store, dir)
	resp := runHashlineEdit(t, tool, "sess", "[a.txt#"+tag+"]\nMV sub/b.txt")
	require.False(t, resp.IsError, resp.Content)
	require.NoFileExists(t, src)
	got, _ := os.ReadFile(filepath.Join(dir, "sub", "b.txt"))
	require.Equal(t, "x\n", string(got))
}

func TestHashlineEditSeenLinesProvenance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "big.txt")
	content := "l1\nl2\nl3\nl4\nl5\n"
	require.NoError(t, os.WriteFile(file, []byte(content), 0o644))
	store := hashline.NewStore()
	abs, _ := filepath.Abs(file)
	// Simulate a windowed read that only displayed lines 1-2.
	store.Record("sess", abs, content, []int{1, 2})

	tool := hashlineTestTool(store, dir)
	tag := hashline.ComputeFileHash(content)

	// Editing an unseen line (5) is rejected.
	resp := runHashlineEdit(t, tool, "sess", "[big.txt#"+tag+"]\nSWAP 5.=5:\n+L5")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "did not display")

	// Editing a seen line (2) is allowed.
	resp = runHashlineEdit(t, tool, "sess", "[big.txt#"+tag+"]\nSWAP 2.=2:\n+L2")
	require.False(t, resp.IsError, resp.Content)
}

func TestHashlineEditRecoversNonConflictingDrift(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	base := "line1\nline2\nline3\nline4\n"
	require.NoError(t, os.WriteFile(file, []byte(base), 0o644))

	store := hashline.NewStore()
	abs, _ := filepath.Abs(file)
	baseTag := store.Record("sess", abs, base, []int{1, 2, 3, 4})

	// The file drifts on disk (line1 changed by something else) after the read.
	require.NoError(t, os.WriteFile(file, []byte("LINE1\nline2\nline3\nline4\n"), 0o644))

	tool := hashlineTestTool(store, dir)
	// Edit anchored on the stale baseTag, targeting line3 (untouched by drift).
	resp := runHashlineEdit(t, tool, "sess", "[a.txt#"+baseTag+"]\nSWAP 3.=3:\n+LINE3")
	require.False(t, resp.IsError, resp.Content)

	got, _ := os.ReadFile(file)
	require.Equal(t, "LINE1\nline2\nLINE3\nline4\n", string(got), "drift + edit should merge")
	require.Contains(t, resp.Content, "merged onto the current version")
}

func TestHashlineEditRejectsConflictingDrift(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	base := "line1\nline2\nline3\n"
	require.NoError(t, os.WriteFile(file, []byte(base), 0o644))

	store := hashline.NewStore()
	abs, _ := filepath.Abs(file)
	baseTag := store.Record("sess", abs, base, []int{1, 2, 3})

	// Drift changes the SAME line the edit targets -> conflict -> reject.
	require.NoError(t, os.WriteFile(file, []byte("line1\nDISK\nline3\n"), 0o644))

	tool := hashlineTestTool(store, dir)
	resp := runHashlineEdit(t, tool, "sess", "[a.txt#"+baseTag+"]\nSWAP 2.=2:\n+OURS")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "changed since")

	got, _ := os.ReadFile(file)
	require.Equal(t, "line1\nDISK\nline3\n", string(got), "conflicting edit must not write")
}
