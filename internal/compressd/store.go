package compressd

import "sync"

// RetrievalStore holds the original, uncompressed content of tool-result
// messages that were replaced with a short summary + compressed text
// before being sent to the model, keyed by a generated placeholder ID.
// It lets the retrieve_full_output tool return the original text if the
// model later needs it.
//
// State is keyed by session ID, mirroring
// [github.com/charmbracelet/crush/internal/skills.LoadedStore]: it is
// held only in memory for the process lifetime and is never persisted.
// Safe for concurrent use.
type RetrievalStore struct {
	mu        sync.RWMutex
	bySession map[string]map[string]string
}

// NewRetrievalStore creates an empty store.
func NewRetrievalStore() *RetrievalStore {
	return &RetrievalStore{bySession: make(map[string]map[string]string)}
}

// Put records the original content for id within sessionID. No-op on a
// nil store or empty sessionID/id.
func (s *RetrievalStore) Put(sessionID, id, content string) {
	if s == nil || sessionID == "" || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.bySession[sessionID]
	if m == nil {
		m = make(map[string]string)
		s.bySession[sessionID] = m
	}
	m[id] = content
}

// Get returns the original content stored for id within sessionID, if
// any. Safe on a nil store.
func (s *RetrievalStore) Get(sessionID, id string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.bySession[sessionID]
	if m == nil {
		return "", false
	}
	content, ok := m[id]
	return content, ok
}

// Clear discards every stored entry for sessionID.
func (s *RetrievalStore) Clear(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.bySession, sessionID)
	s.mu.Unlock()
}
