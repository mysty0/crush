// Package memory is Crush's native, self-updating long-term memory: durable
// facts captured automatically and recalled automatically, stored per project
// in Crush's SQLite database. It is pure Go (no CGO): lexical retrieval uses
// SQLite's built-in FTS5 full-text index, scored with recency and importance.
//
// See docs/memory-design.md for the full design.
package memory

import (
	"fmt"
	"hash/fnv"
	"path/filepath"
	"strings"
)

// Kind classifies a memory. Kinds are advisory; retrieval treats them equally.
const (
	KindFact       = "fact"
	KindPreference = "preference"
	KindConvention = "convention"
	KindDecision   = "decision"
)

// ScopeGlobal holds cross-project facts (user preferences that apply anywhere).
const ScopeGlobal = "global"

// Memory is one stored fact.
type Memory struct {
	ID           string  `json:"id"`
	Scope        string  `json:"scope"`
	Content      string  `json:"content"`
	Kind         string  `json:"kind"`
	Importance   float64 `json:"importance"`
	Source       string  `json:"source"`
	CreatedAt    int64   `json:"created_at"`
	LastUsedAt   int64   `json:"last_used_at,omitempty"`
	UseCount     int64   `json:"use_count"`
	SupersededBy string  `json:"superseded_by,omitempty"`
}

// Hit is a recalled memory with its computed relevance score.
type Hit struct {
	Memory
	Score float64 `json:"score"`
}

// ProjectScope returns a stable per-project scope key derived from the absolute
// workspace path, so memories are isolated per repository.
func ProjectScope(workingDir string) string {
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		abs = workingDir
	}
	abs = filepath.Clean(abs)
	h := fnv.New64a()
	_, _ = h.Write([]byte(abs))
	return fmt.Sprintf("proj_%x", h.Sum64())
}

// normalizeContent lowercases and collapses whitespace for duplicate detection.
func normalizeContent(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// clampImportance bounds an importance value to [0, 1].
func clampImportance(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
