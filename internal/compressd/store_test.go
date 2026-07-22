package compressd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetrievalStore_PutGet(t *testing.T) {
	t.Parallel()

	s := NewRetrievalStore()
	_, ok := s.Get("session-1", "id-1")
	require.False(t, ok)

	s.Put("session-1", "id-1", "original content")
	got, ok := s.Get("session-1", "id-1")
	require.True(t, ok)
	require.Equal(t, "original content", got)

	// A different session must not see it.
	_, ok = s.Get("session-2", "id-1")
	require.False(t, ok)
}

func TestRetrievalStore_Clear(t *testing.T) {
	t.Parallel()

	s := NewRetrievalStore()
	s.Put("session-1", "id-1", "content")
	s.Clear("session-1")

	_, ok := s.Get("session-1", "id-1")
	require.False(t, ok)
}

func TestRetrievalStore_NilSafe(t *testing.T) {
	t.Parallel()

	var s *RetrievalStore
	s.Put("session-1", "id-1", "content") // must not panic
	_, ok := s.Get("session-1", "id-1")
	require.False(t, ok)
	s.Clear("session-1") // must not panic
}
