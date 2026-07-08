package hashline

import (
	"sync"
	"time"
)

// Snapshot is one full-file version observed by a producer (the read tool).
type Snapshot struct {
	Path       string
	Text       string // LF-normalized full file text as observed
	Hash       string // ComputeFileHash(Text)
	SeenLines  map[int]struct{}
	RecordedAt time.Time
}

const defaultMaxVersionsPerPath = 4

// Store binds section tags to the exact content that minted them, per session.
// It is in-memory only; reads repopulate it, so it needs no persistence. Safe
// for concurrent use.
type Store struct {
	mu                 sync.Mutex
	sessions           map[string]map[string][]*Snapshot // session -> path -> newest-first versions
	maxVersionsPerPath int
}

// NewStore returns an empty snapshot store.
func NewStore() *Store {
	return &Store{
		sessions:           map[string]map[string][]*Snapshot{},
		maxVersionsPerPath: defaultMaxVersionsPerPath,
	}
}

// Record stores the full normalized text of path under session and returns its
// content tag. seen holds the 1-indexed lines the producer displayed; they
// merge across reads of identical text. Recording byte-identical content
// refreshes the existing version and reuses its tag.
func (s *Store) Record(session, path, text string, seen []int) string {
	hash := ComputeFileHash(text)
	s.mu.Lock()
	defer s.mu.Unlock()

	paths := s.sessions[session]
	if paths == nil {
		paths = map[string][]*Snapshot{}
		s.sessions[session] = paths
	}
	history := paths[path]

	// Fuse onto an existing byte-identical version.
	for i, snap := range history {
		if snap.Text == text {
			mergeSeen(snap, seen)
			snap.RecordedAt = time.Now()
			if i != 0 {
				history = append(history[:i], history[i+1:]...)
				history = append([]*Snapshot{snap}, history...)
				paths[path] = history
			}
			return hash
		}
	}

	snap := &Snapshot{Path: path, Text: text, Hash: hash, SeenLines: map[int]struct{}{}, RecordedAt: time.Now()}
	mergeSeen(snap, seen)
	history = append([]*Snapshot{snap}, history...)
	if len(history) > s.maxVersionsPerPath {
		history = history[:s.maxVersionsPerPath]
	}
	paths[path] = history
	return hash
}

// Head returns the most recently recorded version for path, if any.
func (s *Store) Head(session, path string) (*Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	history := s.sessions[session][path]
	if len(history) == 0 {
		return nil, false
	}
	return history[0], true
}

// ByHash returns the recorded version for path whose tag equals hash, if any.
func (s *Store) ByHash(session, path, hash string) (*Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, snap := range s.sessions[session][path] {
		if snap.Hash == hash {
			return snap, true
		}
	}
	return nil, false
}

// Invalidate drops the version history for a single path.
func (s *Store) Invalidate(session, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions[session], path)
}

// Relocate moves retained version history from one path to another (used by
// file moves so tags minted from reads of the source stay valid).
func (s *Store) Relocate(session, from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths := s.sessions[session]
	if paths == nil {
		return
	}
	if history, ok := paths[from]; ok {
		paths[to] = history
		delete(paths, from)
	}
}

func mergeSeen(snap *Snapshot, seen []int) {
	if snap.SeenLines == nil {
		snap.SeenLines = map[int]struct{}{}
	}
	for _, line := range seen {
		snap.SeenLines[line] = struct{}{}
	}
}
