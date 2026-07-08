package skills

import (
	"fmt"
	"strings"
	"sync"
)

// LoadedStore tracks skills that have been activated within a session so
// their instructions can be re-injected on every turn (and thus survive
// context summarization). It is the mechanism that makes an activated
// skill "stick" across turns instead of fading after its one-time load.
//
// State is keyed by session ID and held only in memory for the process
// lifetime; it is intentionally not persisted. Safe for concurrent use.
type LoadedStore struct {
	mu        sync.RWMutex
	bySession map[string]*sessionSkills
}

// sessionSkills holds the activated skills for a single session, keeping
// insertion order stable so the injected block is deterministic.
//
// gen increments whenever the active set changes (a skill added/removed)
// or a summarization occurs; injectedGen records the gen value last
// delivered to the model. The injected block is (re)sent whenever
// gen > injectedGen, which happens on activation and after each
// summarization — cheap steady state, guaranteed survival across
// compaction.
type sessionSkills struct {
	order        []string
	instructions map[string]string
	gen          uint64
	injectedGen  uint64
}

// NewLoadedStore creates an empty store.
func NewLoadedStore() *LoadedStore {
	return &LoadedStore{bySession: make(map[string]*sessionSkills)}
}

// Add activates a skill for the given session, recording its instructions
// for later injection. Re-adding an already active skill refreshes its
// instructions without duplicating it. No-op on a nil store or empty
// session/name.
func (s *LoadedStore) Add(sessionID, name, instructions string) {
	if s == nil || sessionID == "" || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.bySession[sessionID]
	if sess == nil {
		sess = &sessionSkills{instructions: make(map[string]string)}
		s.bySession[sessionID] = sess
	}
	if _, ok := sess.instructions[name]; !ok {
		sess.order = append(sess.order, name)
	}
	sess.instructions[name] = instructions
	// The active set changed, so the block must be (re)delivered.
	sess.gen++
}

// Remove deactivates a single skill for the session. No-op if not active.
func (s *LoadedStore) Remove(sessionID, name string) {
	if s == nil || sessionID == "" || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.bySession[sessionID]
	if sess == nil {
		return
	}
	if _, ok := sess.instructions[name]; !ok {
		return
	}
	delete(sess.instructions, name)
	for i, n := range sess.order {
		if n == name {
			sess.order = append(sess.order[:i], sess.order[i+1:]...)
			break
		}
	}
	if len(sess.order) == 0 {
		delete(s.bySession, sessionID)
	}
}

// Clear deactivates every skill for the session.
func (s *LoadedStore) Clear(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.bySession, sessionID)
	s.mu.Unlock()
}

// Bump marks the session's active skills as needing re-injection on the
// next turn. Called after a summarization compacts the conversation so
// the instructions survive. No-op when no skills are active.
func (s *LoadedStore) Bump(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	if sess := s.bySession[sessionID]; sess != nil {
		sess.gen++
	}
	s.mu.Unlock()
}

// TakeInjection reports whether the session's active-skill block should
// be injected this turn, and if so records that it was delivered so it is
// not resent until the set changes or a summarization occurs. Returns
// false when no skills are active or the current block was already
// delivered.
func (s *LoadedStore) TakeInjection(sessionID string) bool {
	if s == nil || sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.bySession[sessionID]
	if sess == nil || len(sess.order) == 0 {
		return false
	}
	if sess.gen == sess.injectedGen {
		return false
	}
	sess.injectedGen = sess.gen
	return true
}

// Names returns the active skill names for the session in activation
// order. Safe on a nil store.
func (s *LoadedStore) Names(sessionID string) []string {
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess := s.bySession[sessionID]
	if sess == nil || len(sess.order) == 0 {
		return nil
	}
	return append([]string(nil), sess.order...)
}

// PromptXML builds the injection block containing every active skill's
// instructions for the session, or an empty string when none are active.
// The block is appended to the model context each turn so the skill
// remains in effect until explicitly deactivated.
func (s *LoadedStore) PromptXML(sessionID string) string {
	if s == nil || sessionID == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess := s.bySession[sessionID]
	if sess == nil || len(sess.order) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<active_skills>\n")
	sb.WriteString("The following skills are active for this conversation. Follow their ")
	sb.WriteString("instructions on every response until the user asks to deactivate them ")
	sb.WriteString("(e.g. \"stop caveman\", \"normal mode\").\n")
	for _, name := range sess.order {
		sb.WriteString("  <skill>\n")
		fmt.Fprintf(&sb, "    <name>%s</name>\n", escape(name))
		sb.WriteString("    <instructions>\n")
		sb.WriteString(escape(sess.instructions[name]))
		sb.WriteString("\n    </instructions>\n")
		sb.WriteString("  </skill>\n")
	}
	sb.WriteString("</active_skills>")
	return sb.String()
}
