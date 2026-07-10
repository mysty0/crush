package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func runAstGrep(t *testing.T, dir, pattern, path string) fantasy.ToolResponse {
	t.Helper()
	tool := NewAstGrepTool(dir)
	params := AstGrepParams{Pattern: pattern, Path: path}
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Name: AstGrepToolName, Input: string(raw)})
	require.NoError(t, err)
	return resp
}

func TestAstGrepLocatesRightOccurrence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Two similar optional-chain sites in different functions; only the
	// pattern-matching one should be reported, with its enclosing symbol.
	src := `export function handleRuntime(gem: any) {
  const runtimeDeps = gem.dependencies.runtime;
  return runtimeDeps;
}

export function handleDev(gem: any) {
  const devDeps = gem.dependencies?.development;
  return devDeps;
}
`
	file := filepath.Join(dir, "rubygems.ts")
	require.NoError(t, os.WriteFile(file, []byte(src), 0o644))

	resp := runAstGrep(t, dir, "$OBJ.dependencies.$FIELD", "rubygems.ts")
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "rubygems.ts")
	require.Contains(t, resp.Content, "handleRuntime") // enclosing symbol reported
	require.Contains(t, resp.Content, "2:")            // line of the non-optional access
	require.NotContains(t, resp.Content, "handleDev")  // the ?. site must NOT match
}

func TestAstGrepNoMatches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.ts"), []byte("const x = 1;\n"), 0o644))
	resp := runAstGrep(t, dir, "$A || $B", "a.ts")
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No matches")
}

func TestAstGrepDirectoryWalk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.ts"), []byte("const x = a || b;\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.ts"), []byte("const y = c || d;\n"), 0o644))
	resp := runAstGrep(t, dir, "$A || $B", ".")
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "a.ts")
	require.Contains(t, resp.Content, "b.ts")
}

func TestAstGrepEmptyPattern(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resp := runAstGrep(t, dir, "   ", ".")
	require.True(t, resp.IsError)
}
