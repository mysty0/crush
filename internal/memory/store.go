package memory

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// dedupeThreshold is the token-set Jaccard similarity at/above which a new
// memory is treated as a restatement of an existing one and merged rather than
// inserted.
const dedupeThreshold = 0.85

// recencyHalfLifeHours controls how fast an unused memory's recency weight
// decays. 30 days: long-term facts stay relevant for weeks.
const recencyHalfLifeHours = 24 * 30

// Store persists and retrieves memories in a SQLite database using FTS5.
type Store struct {
	db *sql.DB
}

// NewStore returns a memory store over db. Call Init before use.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Init creates the memory schema if absent. It is idempotent and safe to call
// on every startup. The schema is self-managed (not a sqlc/goose migration) so
// the FTS5 virtual table stays out of the generated query layer.
func (s *Store) Init(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS memories (
  id            TEXT PRIMARY KEY,
  scope         TEXT NOT NULL,
  content       TEXT NOT NULL,
  kind          TEXT NOT NULL DEFAULT 'fact',
  importance    REAL NOT NULL DEFAULT 0.5,
  source        TEXT,
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER,
  use_count     INTEGER NOT NULL DEFAULT 0,
  superseded_by TEXT
);
CREATE INDEX IF NOT EXISTS idx_memories_scope ON memories(scope, superseded_by);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, content='memories', content_rowid='rowid', tokenize='porter unicode61');
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
  INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("memory: init schema: %w", err)
	}
	return nil
}

// RememberParams describes a memory to store.
type RememberParams struct {
	Scope       string
	Content     string
	Kind        string
	Importance  float64
	Source      string
	MaxPerScope int // 0 = no eviction
}

// Remember stores a memory, merging it into a near-duplicate in the same scope
// when one exists (keeping the higher importance) instead of adding a row. It
// returns the resulting memory and whether a new row was created.
func (s *Store) Remember(ctx context.Context, p RememberParams) (Memory, bool, error) {
	content := strings.TrimSpace(p.Content)
	if content == "" {
		return Memory{}, false, fmt.Errorf("memory: empty content")
	}
	if p.Kind == "" {
		p.Kind = KindFact
	}
	if p.Importance == 0 {
		p.Importance = 0.5
	}
	p.Importance = clampImportance(p.Importance)
	now := time.Now().Unix()

	// Dedupe: merge into an existing near-duplicate in the same scope.
	existing, err := s.liveInScope(ctx, p.Scope)
	if err != nil {
		return Memory{}, false, err
	}
	for _, m := range existing {
		if normalizeContent(m.Content) == normalizeContent(content) || jaccard(m.Content, content) >= dedupeThreshold {
			imp := math.Max(m.Importance, p.Importance)
			if _, err := s.db.ExecContext(ctx,
				`UPDATE memories SET importance=?, source=?, created_at=? WHERE id=?`,
				imp, p.Source, now, m.ID); err != nil {
				return Memory{}, false, fmt.Errorf("memory: merge: %w", err)
			}
			m.Importance, m.Source, m.CreatedAt = imp, p.Source, now
			return m, false, nil
		}
	}

	m := Memory{
		ID: uuid.NewString(), Scope: p.Scope, Content: content, Kind: p.Kind,
		Importance: p.Importance, Source: p.Source, CreatedAt: now,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (id, scope, content, kind, importance, source, created_at, use_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		m.ID, m.Scope, m.Content, m.Kind, m.Importance, m.Source, m.CreatedAt); err != nil {
		return Memory{}, false, fmt.Errorf("memory: insert: %w", err)
	}
	if p.MaxPerScope > 0 {
		if err := s.evict(ctx, p.Scope, p.MaxPerScope); err != nil {
			return Memory{}, false, err
		}
	}
	return m, true, nil
}

// Recall returns the memories in the given scopes most relevant to query,
// ranked by FTS relevance blended with recency and importance. It bumps
// use_count / last_used_at on the returned rows.
func (s *Store) Recall(ctx context.Context, scopes []string, query string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 8
	}
	match := ftsMatchQuery(query)
	if match == "" || len(scopes) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(scopes)), ",")
	args := []any{match}
	for _, sc := range scopes {
		args = append(args, sc)
	}
	// Over-fetch a candidate pool, then re-rank in Go.
	args = append(args, limit*4)
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.scope, m.content, m.kind, m.importance, m.source,
		       m.created_at, COALESCE(m.last_used_at,0), m.use_count,
		       bm25(memories_fts) AS rank
		FROM memories_fts
		JOIN memories m ON m.rowid = memories_fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.superseded_by IS NULL
		  AND m.scope IN (`+ph+`)
		ORDER BY rank
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: recall: %w", err)
	}
	defer rows.Close()

	type cand struct {
		m    Memory
		bm25 float64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.m.ID, &c.m.Scope, &c.m.Content, &c.m.Kind, &c.m.Importance,
			&c.m.Source, &c.m.CreatedAt, &c.m.LastUsedAt, &c.m.UseCount, &c.bm25); err != nil {
			return nil, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// Normalize bm25 (more negative = better) to a [0,1] relevance across the pool.
	best, worst := cands[0].bm25, cands[0].bm25
	for _, c := range cands {
		best = math.Min(best, c.bm25)
		worst = math.Max(worst, c.bm25)
	}
	now := time.Now().Unix()
	hits := make([]Hit, 0, len(cands))
	for _, c := range cands {
		rel := 1.0
		if worst > best {
			rel = (worst - c.bm25) / (worst - best)
		}
		ageHours := float64(now-c.m.CreatedAt) / 3600.0
		recency := math.Exp(-math.Ln2 * ageHours / recencyHalfLifeHours)
		score := rel*0.6 + c.m.Importance*0.2 + recency*0.2
		hits = append(hits, Hit{Memory: c.m, Score: score})
	}
	sortHitsDesc(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	s.markUsed(ctx, hits, now)
	return hits, nil
}

// Forget deletes memories in a scope by exact id or by matching query text.
// Returns the number of rows removed.
func (s *Store) Forget(ctx context.Context, scope, idOrQuery string) (int, error) {
	if res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id=? AND scope=?`, idOrQuery, scope); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			return int(n), nil
		}
	}
	hits, err := s.Recall(ctx, []string{scope}, idOrQuery, 5)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range hits {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id=?`, h.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Supersede soft-deletes oldID, pointing it at newID; superseded rows are
// excluded from recall but retained for audit.
func (s *Store) Supersede(ctx context.Context, oldID, newID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET superseded_by=? WHERE id=?`, newID, oldID)
	return err
}

// List returns the live memories in a scope, newest first.
func (s *Store) List(ctx context.Context, scope string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, content, kind, importance, source, created_at,
		       COALESCE(last_used_at,0), use_count
		FROM memories
		WHERE scope=? AND superseded_by IS NULL
		ORDER BY created_at DESC LIMIT ?`, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// Clear removes every memory in a scope.
func (s *Store) Clear(ctx context.Context, scope string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE scope=?`, scope)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// liveInScope returns non-superseded memories in a scope.
func (s *Store) liveInScope(ctx context.Context, scope string) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, content, kind, importance, source, created_at,
		       COALESCE(last_used_at,0), use_count
		FROM memories WHERE scope=? AND superseded_by IS NULL`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// evict trims a scope to maxPerScope live rows, dropping the lowest-weight
// memories (importance * recency * log(use_count+2)).
func (s *Store) evict(ctx context.Context, scope string, maxPerScope int) error {
	mems, err := s.liveInScope(ctx, scope)
	if err != nil {
		return err
	}
	if len(mems) <= maxPerScope {
		return nil
	}
	now := time.Now().Unix()
	weight := func(m Memory) float64 {
		ageHours := float64(now-m.CreatedAt) / 3600.0
		recency := math.Exp(-math.Ln2 * ageHours / recencyHalfLifeHours)
		return m.Importance * recency * math.Log(float64(m.UseCount)+2)
	}
	sortMemsByWeightAsc(mems, weight)
	for _, m := range mems[:len(mems)-maxPerScope] {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id=?`, m.ID); err != nil {
			return err
		}
	}
	return nil
}

// markUsed bumps use_count / last_used_at for recalled memories.
func (s *Store) markUsed(ctx context.Context, hits []Hit, now int64) {
	for _, h := range hits {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE memories SET use_count=use_count+1, last_used_at=? WHERE id=?`, now, h.ID)
	}
}

func scanMemories(rows *sql.Rows) ([]Memory, error) {
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Scope, &m.Content, &m.Kind, &m.Importance,
			&m.Source, &m.CreatedAt, &m.LastUsedAt, &m.UseCount); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
