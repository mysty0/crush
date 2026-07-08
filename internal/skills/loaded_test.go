package skills

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadedStore_AddInjectAndPersist(t *testing.T) {
	t.Parallel()

	store := NewLoadedStore()
	const sess = "session-1"

	require.Empty(t, store.PromptXML(sess))
	require.Nil(t, store.Names(sess))

	store.Add(sess, "caveman", "Talk terse.")

	xml := store.PromptXML(sess)
	require.Contains(t, xml, "<active_skills>")
	require.Contains(t, xml, "<name>caveman</name>")
	require.Contains(t, xml, "Talk terse.")
	require.Equal(t, []string{"caveman"}, store.Names(sess))

	// Re-adding refreshes instructions without duplicating the entry.
	store.Add(sess, "caveman", "Talk very terse.")
	require.Equal(t, []string{"caveman"}, store.Names(sess))
	require.Contains(t, store.PromptXML(sess), "Talk very terse.")
	require.Equal(t, 1, strings.Count(store.PromptXML(sess), "<skill>"))
}

func TestLoadedStore_IsolatedPerSession(t *testing.T) {
	t.Parallel()

	store := NewLoadedStore()
	store.Add("a", "caveman", "x")

	require.NotEmpty(t, store.PromptXML("a"))
	require.Empty(t, store.PromptXML("b"), "skills must not leak across sessions")
}

func TestLoadedStore_RemoveAndClear(t *testing.T) {
	t.Parallel()

	store := NewLoadedStore()
	const sess = "s"
	store.Add(sess, "one", "1")
	store.Add(sess, "two", "2")
	require.Equal(t, []string{"one", "two"}, store.Names(sess))

	store.Remove(sess, "one")
	require.Equal(t, []string{"two"}, store.Names(sess))

	store.Clear(sess)
	require.Nil(t, store.Names(sess))
	require.Empty(t, store.PromptXML(sess))
}

func TestLoadedStore_NilSafe(t *testing.T) {
	t.Parallel()

	var store *LoadedStore
	require.NotPanics(t, func() {
		store.Add("s", "n", "i")
		store.Remove("s", "n")
		store.Clear("s")
		store.Bump("s")
		require.False(t, store.TakeInjection("s"))
		require.Nil(t, store.Names("s"))
		require.Empty(t, store.PromptXML("s"))
	})
}

func TestLoadedStore_InjectionGate(t *testing.T) {
	t.Parallel()

	store := NewLoadedStore()
	const sess = "s"

	// Nothing active: never inject.
	require.False(t, store.TakeInjection(sess))

	// Activation makes it injectable exactly once (generation gate).
	store.Add(sess, "caveman", "x")
	require.True(t, store.TakeInjection(sess), "should inject on activation")
	require.False(t, store.TakeInjection(sess), "must not re-inject unchanged block")

	// A summarization bumps the generation, so it injects again once.
	store.Bump(sess)
	require.True(t, store.TakeInjection(sess), "should re-inject after summarize")
	require.False(t, store.TakeInjection(sess))

	// Adding another skill changes the set and re-arms injection.
	store.Add(sess, "jq", "y")
	require.True(t, store.TakeInjection(sess))
	require.False(t, store.TakeInjection(sess))
}

func TestLoadedStore_BumpNoActiveSkillsIsNoop(t *testing.T) {
	t.Parallel()

	store := NewLoadedStore()
	store.Bump("s")
	require.False(t, store.TakeInjection("s"))
}
