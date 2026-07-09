# Self-updating memory — design

## Goal

Crush should remember durable facts across sessions and surface them
automatically, with **zero user bookkeeping**. The user never runs a
"remember this" command and never manages a memory store; they just work, and
over time Crush stops re-asking things it already learned (project conventions,
build/test commands, user preferences, where things live, past decisions).

Two properties define success:

1. **Self-updating capture** — memories are extracted automatically from the
   conversation; the user does nothing.
2. **Automatic recall** — relevant memories are injected into the model's
   context by the *system*, before the turn runs. The model does not have to
   decide to call a `recall` tool — recall happens whether or not the model
   thinks to ask.

This is the key difference from a plain "memory tool": tools only fire when the
model chooses to use them. For *transparent* memory, retrieval must be
automatic.

Non-goals: a hosted/cross-device memory service (the existing `mem0` MCP covers
that), and within-session context compaction (Crush already summarizes long
conversations — that is short-term, this is long-term).

## Prior art (oh-my-pi)

oh-my-pi's memory is three decoupled subsystems; we borrow the useful shape and
drop the rest:

- **Autolearn** — a turn-end nudge asks the model to `learn` durable facts, plus
  a background two-phase LLM pipeline that extracts facts from past session
  rollouts and consolidates them per project into `MEMORY.md` +
  `memory_summary.md`, injected into the system prompt (~5k-token budget,
  snapshotted once per session so the prompt cache stays stable).
- **mnemopi** — a local SQLite retriever: FTS5 (BM25) + keyword overlap +
  *optional* dense-vector cosine, scored with recency decay (72h half-life),
  importance, veracity, and tier weights, with MMR for diversity. Embeddings are
  optional; the pure-lexical path is fully functional.
- **snapcompact** — within-session compaction (mechanical PNG rasterization for
  vision models). Out of scope for us.

What we take: (a) automatic background extraction + consolidation, (b) a
lexical-first SQLite retriever, (c) cache-stable injection. What we change:
oh-my-pi injects the *whole* consolidated summary and relies on model-invoked
`recall` for relevance. To meet "automatically recall," we make injection
**query-relevant and automatic** every turn.

## Fit with Crush

Everything needed already exists in the codebase, CGO-free:

- **Storage** — Crush uses `modernc.org/sqlite` (pure Go) with sqlc-generated
  queries (`internal/db/`), timestamped migrations
  (`internal/db/migrations/`), and raw SQL in `internal/db/sql/`. A `memories`
  table + an FTS5 virtual table is one migration + one `.sql` file + `sqlc`
  regen. (modernc ships FTS5 enabled — verify on the pinned version in a test.)
- **Automatic injection** — `agent.go:864` already appends a trailing system
  message from `activeSkillsFor(sessionID)` *after* the cached prompt prefix,
  precisely so it is cache-friendly. Memory injection is the same hook with a
  second provider function.
- **Triggers** — `internal/pubsub` + the coordinator's turn lifecycle
  (`agent_end`-equivalent) give us the end-of-turn/end-of-session events to
  schedule background extraction. Crush's hooks engine is an alternative trigger
  surface.
- **Config** — add a `Memory` block to `config.Options` (mirrors the new `Read`
  / `Edit` options), with an accessor and a schema entry.

## Architecture

```
            ┌─────────────────────────  a turn runs  ─────────────────────────┐
            │                                                                  │
 user msg ──┤ 1. RETRIEVE (automatic, no model call)                          │
            │    query = latest user msg (+ recent context)                    │
            │    FTS5 + recency/importance score → top-k memories              │
            │ 2. INJECT as a trailing system block (cache-aware)               │
            │ 3. model responds using the injected facts                       │
            └───────────────────────────────┬──────────────────────────────────┘
                                            │ turn/session end (pubsub)
                                            ▼
            ┌──────────────────────  background, async  ──────────────────────┐
            │ 4. EXTRACT durable facts from the transcript (cheap LLM, JSON)   │
            │ 5. CONSOLIDATE: dedupe / supersede stale / cap per scope         │
            │ 6. persist to SQLite (+ FTS index)                               │
            └──────────────────────────────────────────────────────────────────┘
```

Capture (4–6) is fully automatic and off the hot path. Recall (1–2) is
automatic and cheap (pure SQL, no model call).

### Storage

One SQLite table plus an FTS5 mirror, in Crush's existing DB.

```sql
CREATE TABLE memories (
  id           TEXT PRIMARY KEY,          -- uuid
  scope        TEXT NOT NULL,             -- 'global' | project key (workdir hash)
  content      TEXT NOT NULL,             -- the fact, one self-contained sentence
  kind         TEXT NOT NULL DEFAULT 'fact', -- fact | preference | convention | decision
  importance   REAL NOT NULL DEFAULT 0.5, -- 0..1
  source       TEXT,                      -- 'auto' | 'tool' | session id
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  use_count    INTEGER NOT NULL DEFAULT 0,
  superseded_by TEXT,                     -- id of the memory that replaced this
  embedding    BLOB                       -- optional; null in lexical-only mode
);
CREATE VIRTUAL TABLE memories_fts USING fts5(content, content='memories', content_rowid='rowid');
-- + AFTER INSERT/UPDATE/DELETE triggers to keep the FTS index in sync.
```

**Scoping.** Default scope is the **project** (a stable hash of the workspace
root, like oh-my-pi's per-cwd dirs). A `global` scope holds cross-project user
preferences ("prefers tabs", "always run gofumpt"). Retrieval reads
`scope IN ('global', <project>)`. Non-superseded rows only.

### Retrieval + automatic injection (the "auto-recall" core)

Before each turn, `MemoryService.Relevant(scope, query, budget)`:

1. Build a query from the latest user message (and optionally the last
   assistant turn). Tokenize, drop stopwords.
2. `SELECT ... FROM memories_fts MATCH ? ...` → BM25 rank, normalized to `[0,1]`.
3. Score each hit: `score = bm25*0.6 + importance*0.2 + recency*0.2`, where
   `recency = exp(-ageHours/72h)`. Superseded rows excluded.
4. Take top-k within a token budget (default ~1000 tokens); bump
   `use_count`/`last_used_at` on returned rows (cheap reinforcement).
5. Render a compact block:

```
<memory>
Known facts about this project (may be stale; current repository state wins):
- The test command is `task test`; single test: `go test ./pkg -run TestX`.
- CGO is disabled (CGO_ENABLED=0); never add cgo deps.
- User prefers concise commit messages, no body unless necessary.
</memory>
```

Injected via a new provider mirroring `activeSkillsFor` — appended as a trailing
system message at `agent.go:864`.

**Two-tier injection for cache stability.** Automatic per-turn injection of a
*changing* block sits *after* the cached prefix, so it does not invalidate the
big system+tools+history cache — but it is re-sent (uncached) each turn. To
bound this:

- **Stable tier** — a small set of high-importance, always-relevant memories
  (global preferences + top project facts) rendered *once per session* and
  memoized (like the skills block), part of the cache-stable prefix.
- **Dynamic tier** — a few query-relevant memories, re-selected each turn,
  appended as the trailing block (small, uncached, cheap).

This keeps the cache-heavy content stable while still surfacing per-turn
relevant facts. Both tiers are advisory and explicitly framed as "may be stale;
current repo state wins" to avoid the model trusting a memory over live code.

### Capture (self-updating)

Two paths, both automatic; no user action:

1. **Background extraction (primary).** On session end (or after N tool calls /
   on compaction), enqueue a job that runs a cheap LLM pass over the recent
   transcript with a strict-JSON extraction prompt:
   > Extract only durable, reusable facts about this project or the user's
   > preferences that will help in a future session. One self-contained
   > sentence each. No transient task state, no secrets. Return `{"memories":
   > [{content, kind, importance}]}` or an empty list.
   Secrets are redacted before persisting. Runs off the hot path, on the small
   model.

2. **In-turn tool (safety net).** A `remember` tool the model *may* call for a
   high-value fact mid-task (e.g. right after discovering the build command).
   Optional; extraction is the net that catches what the model forgets to save.

### Self-cleaning (core — keeping memory correct)

Because capture is automatic and **default-on**, the store *will* accumulate
duplicates, stale facts, and the occasional wrong inference. Cleaning is
therefore a first-class mechanism, not a nice-to-have. Five layers, cheapest
first:

1. **Write-time dedupe.** Before inserting, check for a near-duplicate
   (normalized-text / trigram similarity, or embedding cosine when enabled). On
   a hit, merge instead of adding: keep one row, take the higher importance,
   refresh `created_at`. Prevents the store from growing on repeated facts.
2. **Conflict supersede.** A new fact that contradicts an existing one sets
   `superseded_by` on the old row (soft delete — kept for audit, excluded from
   retrieval). "Most recent wins." Conflict detection is subject-based: two
   memories about the same subject (same key entities / same normalized
   predicate) with different values conflict.
3. **Usage decay + eviction.** Each memory's live weight is
   `importance * recency * log(use_count + 1)` where `recency =
   exp(-ageHours/halflife)`. Memories that are never recalled decay and, once a
   per-scope cap is hit (default 500), the lowest-weight rows are evicted. So
   facts that never prove useful fade on their own — no manual pruning.
4. **Reality check on use.** Memory is advisory ("current repo state wins"). A
   `verify` field records when a fact was last confirmed; when the agent acts on
   a memory and finds it contradicted by live code/output, that memory is
   invalidated (superseded) — cleaning driven by real use, not a schedule.
5. **Periodic consolidation (LLM).** Occasionally (e.g. when a scope exceeds a
   count threshold, or every N sessions), a background small-model pass over one
   scope merges related facts, drops contradictions, and compresses verbose ones
   into 1–3 tight sentences (oh-my-pi's phase 2). Runs off the hot path,
   strict-JSON, secret-redacted, and only *replaces* rows it consolidated.

Layers 1–3 are mechanical and ship first; 4–5 are refinements. Together they
make the store self-limiting: it converges to a small, deduped, mostly-verified
set instead of growing without bound.

On top of the automatic layers, cleaning is also **user-triggerable**:
`crush memory forget <id|query>` and `crush memory clear`, and a `forget` op on
the in-turn tool so the agent can drop a fact it discovers is wrong.

### User controls (present but not required)

Memory "just works," but for trust it must be inspectable and reversible:

- `crush memory list [--scope]`, `crush memory forget <id|query>`,
  `crush memory clear`.
- A one-line notice the first time a memory is captured/used (discoverable, not
  nagging), and memories visible in the session view.
- `options.memory.enabled` (**default on**), secret redaction always on, and a
  global opt-out for users who don't want persisted memory.

## Config

```jsonc
"options": {
  "memory": {
    "enabled": true,
    "scope": "project",          // project | global | both
    "inject_budget_tokens": 1000,
    "capture": "auto",           // auto | tool | off
    "max_per_scope": 500,
    "embeddings": false          // lexical-only by default
  }
}
```

Accessors on `config.Options` (e.g. `MemoryEnabled()`, `MemoryInjectBudget()`),
plus a `schema.json` regen — same pattern as the `read`/`edit` options added
earlier.

## Portability (pure Go, CGO-off)

| Piece | Difficulty | Notes |
|---|---|---|
| SQLite + FTS5 store | Easy | `modernc.org/sqlite` is already the driver; FTS5 is built in. Reuse sqlc + migrations. |
| Lexical retrieval + scoring | Easy | BM25 from FTS5 `rank`; recency/importance are arithmetic. ~deterministic. |
| Automatic injection | Easy | Second provider alongside `activeSkillsFor` at `agent.go:864`; cache treatment already solved for skills. |
| Background extraction | Medium | Scheduled small-model call + JSON parse + redaction; wire to turn/session-end pubsub. |
| Consolidation/eviction | Medium | Similarity + recency to start; LLM pass later. |
| Embeddings (optional) | Medium | Skip for v1 (lexical works). If wanted, a remote embedding HTTP call keeps the binary CGO-free; never link ONNX. |

No CGO, no native modules, no new heavy deps. Embeddings stay behind an
interface and default off.

## Relationship to the mem0 MCP

Crush today can use the external `mem0` MCP (`mcp__mem0__remember/recall`). That
stays for users who want a hosted, cross-tool store. This design is the
**native, offline, zero-setup** alternative: per-project, in Crush's own SQLite,
no server, automatic capture *and* automatic recall (mem0's tools are
model-invoked, so recall is not automatic).

## Phased plan

1. **Store + retrieval + tools + mechanical cleaning.** Migration, sqlc
   queries, `MemoryService`, lexical scoring, and `remember`/`recall`/`forget`
   tools. Include self-cleaning layers 1–3 (write-time dedupe, conflict
   supersede, usage decay + eviction) from the start, so the store is
   self-limiting the moment anything writes to it.
2. **Automatic injection.** Wire the two-tier injection at `agent.go:864`. This
   delivers *automatic recall* — the headline feature. Measure cache/token cost.
3. **Automatic extraction.** Background small-model job on session end;
   strict-JSON prompt + secret redaction. This delivers *self-updating capture*.
   Measure precision (are captured facts durable and correct?) and cost.
4. **Cleaning refinements.** Reality-check-on-use invalidation (layer 4) and the
   periodic LLM consolidation pass (layer 5).
5. **Optional embeddings** behind the interface, for semantic recall.

Phases 2 and 3 are the user's ask: capture the user never thinks about, recall
that happens on its own. Cleaning (phase 1 layers + phase 4) runs throughout so
the default-on store stays correct and bounded.

## Decisions

- **Default-on.** Memory is enabled by default (with a global opt-out), so it
  "just works" without the user thinking about it. This raises the bar on
  cleaning and cost, addressed below.
- **Cleaning is core, not optional.** The five self-cleaning layers keep the
  store deduped, current, and bounded automatically; `forget`/`clear` give the
  user (and the agent) an explicit override. This is what makes default-on safe.
- **Trigger: end-of-session + tool.** Extraction runs at session end
  (cache-cheapest, fewest LLM calls); the in-turn `remember` tool catches urgent
  facts mid-task.

### What to measure before shipping default-on

- **Capture precision** — sample auto-captured memories: what fraction are
  durable and correct vs transient/wrong? Drives the extraction prompt and the
  importance threshold for what gets stored.
- **Recall usefulness** — do injected memories actually get used / help, or just
  add noise? (Track `use_count`; spot-check transcripts.)
- **Cost** — per-turn dynamic injection tokens (uncached) and the per-session
  extraction call. Both must be small; measure with the same eval harness used
  for the read-summary and syntax-gate work.
- **Cleaning efficacy** — does the store converge (dedupe/eviction keeping it
  bounded) rather than grow without limit over many sessions?
