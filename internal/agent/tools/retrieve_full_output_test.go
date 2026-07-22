package tools

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/compressd"
	"github.com/stretchr/testify/require"
)

func retrieveCtx(sessionID string) context.Context {
	return context.WithValue(context.Background(), SessionIDContextKey, sessionID)
}

func TestRetrieveFullOutputTool_ReturnsStoredContent(t *testing.T) {
	t.Parallel()

	store := compressd.NewRetrievalStore()
	store.Put("s1", "abc123", "the original full tool output")
	tool := NewRetrieveFullOutputTool(store)

	resp, err := tool.Run(retrieveCtx("s1"), fantasy.ToolCall{Input: `{"id":"abc123"}`})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "the original full tool output", resp.Content)
}

func TestRetrieveFullOutputTool_UnknownID(t *testing.T) {
	t.Parallel()

	store := compressd.NewRetrievalStore()
	tool := NewRetrieveFullOutputTool(store)

	resp, err := tool.Run(retrieveCtx("s1"), fantasy.ToolCall{Input: `{"id":"nope"}`})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no stored output")
}

func TestRetrieveFullOutputTool_WrongSession(t *testing.T) {
	t.Parallel()

	store := compressd.NewRetrievalStore()
	store.Put("s1", "abc123", "content for s1")
	tool := NewRetrieveFullOutputTool(store)

	resp, err := tool.Run(retrieveCtx("s2"), fantasy.ToolCall{Input: `{"id":"abc123"}`})
	require.NoError(t, err)
	require.True(t, resp.IsError)
}

func TestRetrieveFullOutputTool_MissingID(t *testing.T) {
	t.Parallel()

	store := compressd.NewRetrievalStore()
	tool := NewRetrieveFullOutputTool(store)

	resp, err := tool.Run(retrieveCtx("s1"), fantasy.ToolCall{Input: `{}`})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "missing id")
}
