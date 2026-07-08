# Hashline edit mode — design

Status: **proposal / design doc** (no code yet). Author: design pass from an
investigation of [oh-my-pi](https://github.com/can1357/oh-my-pi)'s hashline
feature and how it maps onto Crush.

## 1. Motivation

Crush's `Edit`/`MultiEdit` tools use **exact-string find-and-replace**: the model
must retype the exact `old_string` verbatim, and we `strings.Index` it in the
file. This format fails constantly on whitespace differences, near-duplicate
lines, and stale content, which produces retry loops that burn tokens and turns.

Hashline replaces the matching strategy with **line-number anchoring guarded by a
content hash**. The model points at line numbers ("replace lines 41–43 with
this") instead of retyping the old text. Line numbers are cheap and unambiguous.
The safety mechanism that makes line-numbering trustworthy is a **4-hex hash of
the whole file** emitted on every `Read`/`grep` as `[path#TAG]`. The edit tool
verifies that tag against the live file *before* applying — a stale file makes the
anchors wrong, so we **reject up front** instead of corrupting code.

oh-my-pi reports large wins from this format (their blog "the harness problem":
Grok Code Fast 6.7% → 68.3% edit success; Grok 4 Fast −61% output tokens) — all
attributable to killing the bad-diff retry loop.

### Goals

- A new `edit` tool that consumes the hashline patch language.
- `Read` emits `[path#TAG]` + `LINE:TEXT` anchors when hashline mode is active.
- Stale edits rejected by content hash before any write.
- Block ops (`SWAP.BLK`/`DEL.BLK`/`INS.BLK.POST`) resolved via tree-sitter.
- Entirely behind a config flag; **default behavior is unchanged**.

### Non-goals (v1)

- Replacing the string `Edit`/`MultiEdit` as the default (kept; flag opt-in).
- Snapshot-based 3-way-merge recovery (deferred — see Phasing).
- `grep` anchor emission (deferred to a later phase).
- Internal-URL / archive / PR read surfaces (out of scope; that is a separate
  omp feature).

## 2. Config flag

Add one field to `config.Options` (`internal/config/config.go:277`):

```go
// EditMode selects the file-editing tool family. "string" (default) keeps the
// exact-match Edit/MultiEdit tools. "hashline" swaps in the line-anchored
// hashline edit tool and changes Read's output format to emit [path#TAG]
// anchors.
EditMode string `json:"edit_mode,omitempty" jsonschema:"description=File editing mode: 'string' (exact find/replace) or 'hashline' (line-anchored patches),enum=string,enum=hashline,default=string"`
```

Default `""`/`"string"` = today's behavior; zero change for existing users.
Defaulting is applied in `Config.setDefaults` (`internal/config/load.go:515`):
if empty, set to `"string"`.

Both the Read format and the edit tool family branch on this single value, so
they are always consistent.

## 3. Architecture

Three coupled pieces plus one isolated tree-sitter dependency:

```
internal/hashline/                 pure patch language (no I/O, no tree-sitter)
  hash.go        ComputeFileHash(text) -> 4-hex; normalization
  format.go      sigils/keywords/header + line formatting
  snapshot.go    Snapshot, Store iface, in-memory session-scoped impl
  tokenizer.go   line-oriented lexer for patch text
  parser.go      patch text -> []Section{Path, Tag, []Edit}; lenient variants
  edit.go        Edit/Cursor/Anchor value types + BlockResolver interface
  apply.go       pure apply(text, []Edit) -> (newText, warnings, error)
  block.go       resolveBlockEdits(edits, text, path, resolver) expansion
  mismatch.go    stale-anchor + out-of-range error formatting w/ context
  diffpreview.go compact post-edit diff snippet

internal/hashline/tsblock/         ISOLATED tree-sitter dependency
  resolver.go    BlockResolver impl over odvcencio/gotreesitter
                 (build-tag-subset grammar set; see §9)

internal/agent/tools/
  view.go        + hashline output branch (gated on EditMode)
  hashline_edit.go   NEW hashline edit tool (params: {input string})
  snapstore.go   (or a service under internal/) wiring the Store per session
```

Key isolation rule: **`internal/hashline` never imports tree-sitter.** It defines
the `BlockResolver` interface; `internal/hashline/tsblock` implements it and is
the single import site for `gotreesitter`. This keeps the core package pure and
unit-testable, and confines the binary-size cost to one package that could later
be swapped or built out.

## 4. Data model

```go
// Snapshot is one full-file version observed by a producer (Read/grep).
type Snapshot struct {
    Path      string
    Text      string          // normalized to LF, BOM-stripped
    Hash      string          // 4-hex tag of Text (see ComputeFileHash)
    SeenLines map[int]struct{} // 1-indexed lines the producer displayed
    RecordedAt time.Time
}

// Store binds section tags to the exact content that minted them, per session.
// In-memory only; Reads repopulate it, so no persistence is needed.
type Store interface {
    Record(session, path, text string, seen []int) (hash string)
    Head(session, path string) (*Snapshot, bool)
    ByHash(session, path, hash string) (*Snapshot, bool)
    Invalidate(session, path string)
    Relocate(session, from, to string)
}
```

The in-memory impl mirrors omp's LRU: per-session, per-path ring of recent
versions (default ~4 versions/path, LRU-bounded paths, global byte ceiling).
It is a **single service instance** injected into both Read (producer) and the
hashline edit tool (consumer), with methods taking `sessionID` — exactly the
pattern `filetracker.Service` uses today (`internal/filetracker/service.go`).

### Relationship to existing staleness mechanisms

Crush already has two staleness guards that the hashline edit tool subsumes:

- `filetracker.LastReadTime` (read-before-edit, mod-time-after-read): hashline's
  content-hash check is strictly stronger — it catches out-of-band edits even
  when mod time is unchanged, and it does *not* false-positive when the content
  is byte-identical. We keep calling `filetracker.RecordRead` (other subsystems
  read it) but gate edits on the hash, not the timestamp.
- `history.Service` (SQLite file versions for undo/diff): unchanged. The
  snapshot store is a separate, lighter, in-memory thing.

## 5. The hash

```go
// ComputeFileHash normalizes then hashes to a 4-hex uppercase tag.
// Normalization (must match between Read producer and Edit consumer):
//   1. strip UTF-8 BOM
//   2. CRLF/CR -> LF
//   3. trim trailing [ \t] from every line
// Then FNV-1a 32-bit, take low 16 bits, format as 4 uppercase hex.
func ComputeFileHash(text string) string
```

Notes:
- omp uses xxHash32 & 0xFFFF; **we do not need format compatibility with omp** —
  only Read↔Edit consistency within Crush. Use stdlib `hash/fnv` (zero new deps).
  (`cespare/xxhash/v2` is already an indirect dep if we ever want it, but it is
  64-bit; fnv is simpler and stdlib.)
- 16-bit tag → collisions are expected and fine: the Store keys on *full text*,
  not the tag. Two different texts sharing a tag are stored as distinct
  snapshots; the tag is only a fast index + a cheap "did the file change" gate.
- Trailing-whitespace normalization means a display that trimmed trailing spaces,
  or a CRLF file, still hashes to the tag the model saw.

## 6. Read tool changes (`view.go`)

When `EditMode == "hashline"`, and the target is a normal mutable text file (not
an image, not a builtin skill file, not `:raw`), replace the `<file>` +
`%6d|line` output with:

```
[relpath#TAG]
41:def alpha():
42:    return 1
```

Flow additions in the handler (after `readTextFile`, `view.go:235`):

1. Normalize the *full* file text (not just the shown window) to compute the tag.
   Note: today `readTextFile` streams only the requested window. For the tag we
   need the whole-file hash. Options:
   - read the whole file for the hash when in hashline mode (simplest; Read
     already caps at `MaxViewSize` 200KB), then slice the window for display; or
   - hash incrementally. Recommend the whole-file read in hashline mode.
2. `hash := store.Record(session, relpath, fullNormalizedText, shownLineNumbers)`
   — record the snapshot and the 1-indexed line numbers actually displayed
   (`SeenLines`, for the provenance check in §8).
3. Emit `formatHashlineHeader(relpath, hash)` then `LINE:TEXT` rows for the shown
   window (line numbers are absolute, `Offset+1`-based).
4. Keep `filetracker.RecordRead` as-is.

String mode is untouched — the whole hashline branch is gated on the flag, so
`EditMode=="string"` users see byte-identical Read output to today (decision #4).

The description template (`view.md.tpl`, rendered via `viewDescription()`) must
be computed from the mode. Since descriptions are built once at construction,
pass `EditMode` into `NewViewTool` and render the hashline-aware description when
active (explaining the `[path#TAG]`/`LINE:TEXT` shape and how anchors feed edit).

## 7. Hashline edit tool

Fantasy tool schemas are fixed per tool, and hashline's parameter is a single
`input` string (not `old_string`/`new_string`). So this is a **separate tool**,
registered *instead of* `Edit`+`MultiEdit` when hashline mode is on.

```go
type HashlineEditParams struct {
    Input string `json:"input" description:"One or more [PATH#TAG] sections with SWAP/DEL/INS ops"`
}
```

### Flow (per call)

1. Parse `input` into `[]Section{Path, Tag, []Edit}` (parser, §8). Reject apply_patch
   / unified-diff contamination with guidance (see omp `edit.md` error catalog).
2. **Preflight all sections** before writing anything (multi-file atomicity):
   for each section:
   a. Resolve `store.ByHash(session, path, tag)`. Missing tag → error
      ("use `[path#tag]` from your latest read"). New-file creation goes through
      `write`, not hashline.
   b. Read live file, normalize (`ToUnixLineEndings`), compute live hash.
      If `liveHash != tag` → **stale**: reject with a mismatch report (§10).
      (Phase 3: attempt snapshot recovery here before rejecting.)
   c. Seen-lines provenance check (§8): any hunk anchored on a line the snapshot
      never displayed → reject, re-read first. **(Phase 3; not in MVP.)**
   d. Expand block ops via `resolveBlockEdits(edits, liveText, path, resolver)`
      (§9). Resolver comes from `tsblock`; nil resolver → block ops error/lower.
   e. `apply(liveText, resolvedEdits)` → `newText`, warnings. Pure, no I/O.
3. If any section failed preflight, return the aggregated error; **nothing is
   written** (matches omp's "partial batch never lands").
4. For each section, request permission (reuse existing
   `permission.CreatePermissionRequest` with `EditPermissionsParams{FilePath,
   OldContent, NewContent}`, `Action:"write"`), restore CRLF if the original was
   CRLF (`ToWindowsLineEndings`), `os.WriteFile`.
5. History + tracking exactly like `edit.go`: `GetByPathAndSession` → `Create`
   if absent → `CreateVersion(old)` if drifted → `CreateVersion(new)`;
   `filetracker.RecordRead`; `notifyLSPs`; append `getDiagnostics`.
6. **Mint a fresh tag** for `newText` and `store.Record(...)` it, then return the
   new `[path#TAG]` header + a compact diff preview (§ diffpreview) so the model
   can anchor the next edit without re-reading.

All the crush-side plumbing (permission, history, filetracker, LSP notify +
diagnostics, CRLF) is identical to `edit.go` — only the *matching + apply* core
is new. The constructor mirrors `NewEditTool` plus the `Store` and a
`BlockResolver`:

```go
func NewHashlineEditTool(
    lspManager *lsp.Manager, permissions permission.Service,
    files history.Service, filetracker filetracker.Service,
    store hashline.Store, resolver hashline.BlockResolver,
    workingDir string,
) fantasy.AgentTool
```

### Tool registration (`coordinator.go` `buildTools`, ~line 731)

Today `NewEditTool` + `NewMultiEditTool` + `NewWriteTool` are appended
unconditionally, then filtered against `agent.AllowedTools`. Change to:

```go
if editMode == "hashline" {
    allTools = append(allTools, tools.NewHashlineEditTool(...))
    // MultiEdit is dropped: hashline is natively multi-file.
} else {
    allTools = append(allTools,
        tools.NewEditTool(...), tools.NewMultiEditTool(...))
}
```

`Write` stays in both modes. **Decision: reuse the name `Edit`** in both modes
for prompt stability (the model always calls `Edit`); the registered tool object
and its JSON schema differ by mode, but the name is constant. The name stays in
`allToolNames()` (config.go) so `DisabledTools` filtering still works unchanged.

## 8. Patch language + parser

Ported from omp `packages/hashline/src/prompt.md` and `edit.md`. Grammar
(line-oriented; a header ending in `:` takes `+TEXT` body rows; `DEL` takes none):

```
[PATH#TAG]              file section header; TAG is 4 uppercase hex
SWAP N.=M:              replace original lines N..M (inclusive) with body
SWAP.BLK N:             replace the syntactic block beginning on line N (§9)
DEL N.=M                delete original lines N..M                 (no body)
DEL.BLK N               delete the block beginning on line N       (no body)
INS.PRE N:              insert body before line N
INS.POST N:             insert body after line N
INS.BLK.POST N:         insert body after the block beginning on line N (§9)
INS.HEAD: / INS.TAIL:   insert body at start / end of file
REM                     delete the whole section file              (Phase 3)
MV DEST                 rename/move the section file to DEST        (Phase 3)
+TEXT                   literal body row; `+` alone = blank line
```

Rules the prompt enforces (verbatim from omp, high value):
- Line numbers refer to the **original** file and never shift as hunks apply.
- Every applied edit mints a **fresh tag and renumbers**; re-anchor on the edit
  response or a fresh Read.
- Ranges cover **only** changed lines; never widen over unchanged lines.
- Pure additions use `INS.*`, never a widened `SWAP`.
- One hunk per range; body is the final content, never an old/new pair.
- No `-` rows; literal `-`/`+` lines are written `+- item` / `++ item`.

### Lenient parsing (accept common model variants)

- `SWAP N:` → `SWAP N.=N:`; `DEL N` → single-line delete.
- `SWAP N-M:`, `SWAP N..M:`, `SWAP N…M:`, `SWAP N M:` → `SWAP N.=M:`.
- Missing trailing colon on `SWAP`/`INS` accepted.
- Bare body rows with no `+` → auto-prefixed with `+` and a warning.
- `*** Begin/End Patch` envelopes silently consumed.
- `@@ ... @@` hunk headers, `*** Update File:` sentinels → rejected with guidance
  to use verb headers.
- Empty `SWAP N.=M:` (no body) → treated as `DEL N.=M`.

### Apply algorithm (`apply.go`)

Pure function `apply(text string, edits []Edit) (string, []Warning, error)`:
1. Split `text` into lines (LF). Validate every anchor is in `[1, len]`
   (out-of-range → error, §10).
2. Normalize replacement boundaries: omp's "boundary-echo repair" drops an
   off-by-one keeper when a multi-line `SWAP` body restates the line just past
   the range (with a warning). *Phase 2 nicety; MVP can skip and rely on the
   prompt.*
3. Bucket edits; apply on the **original** coordinate system (inserts and deletes
   keyed to original line numbers), then materialize the new line list. Inserts
   at a line, deletes of a range, replacements = insert-at-start + delete-range.
   Order within a line is deterministic (before-anchor inserts, then the line,
   then after-anchor inserts).
4. Reject overlapping hunks on the same anchor.
5. Detect no-op (result byte-identical to input) → error; escalate to a hard
   error after N identical no-ops (omp `NOOP_HARD_LIMIT=3`).

### Seen-lines provenance

`store.Record` captured which 1-indexed lines Read actually displayed. A hunk
anchored on a line the snapshot never displayed (e.g. inside an elided summary
region, or beyond the read window) is rejected with "re-read first". This
prevents edits against lines the model never actually saw. **Deferred to Phase
3.** (Crush's Read is windowed, so until this lands the prompt must lean on
"only touch lines your latest Read displayed"; the content-hash gate still
prevents editing a *changed* file, just not an unseen line of an unchanged one.)

## 9. Block ops via tree-sitter

### Dependency: `github.com/odvcencio/gotreesitter` (pure Go, no CGO)

Verified empirically (see Appendix A): builds under `CGO_ENABLED=0`, parses
correctly, and the block resolver primitive works for Go, Python (decorators),
and Markdown (sections). This matters because Crush is hard-locked to
`CGO_ENABLED=0` (`Taskfile.yaml:12`, `.goreleaser.yml:46`), which rules out the
official/`smacker` CGO bindings.

### Binary-size — ship all grammars

**Decision: embed all grammars** (~206). This is the library's *default* embed,
so there is **zero build-tag config to maintain** and no language gaps — block
ops work anywhere tree-sitter has a grammar; unsupported files just fall back to
plain line ops.

Verified sizes (hello-world probe): all grammars **35 MB** vs a 4-grammar subset
**11 MB**. The incremental cost over an empty binary is ~33 MB (runtime ~9 MB +
all blobs ~24 MB), landing Crush's ~109 MB binary at **~140 MB**. That cost is
always paid because the `tsblock` package is compiled in (though only *used* in
hashline mode).

Subsetting via `grammar_subset*` build tags remains available as a future lever
if binary size ever needs trimming (verified: 35 MB → 11 MB), but it is not used
in v1.

### The resolver

`internal/hashline/tsblock` implements `hashline.BlockResolver`:

```go
// ResolveBlock returns the 1-indexed inclusive [start,end] of the syntactic
// block that BEGINS on line N, or ok=false if none.
func ResolveBlock(path, text string, lineN int) (start, end int, ok bool)
```

Algorithm (verified, Appendix A):
1. `grammars.DetectLanguage(path)` → language; unknown language → `ok=false`.
2. Parse `text`; walk the tree for the **outermost named node whose
   `StartPoint().Row == lineN-1`, excluding the tree root**. (Point.Row is
   0-indexed. "Outermost" = largest span among same-row candidates; nodes
   starting at the same position are nested, so largest = shallowest ancestor.)
3. Return its `[StartPoint().Row+1, EndPoint().Row+1]`.
4. Single-line span (`start==end`) → treat as unresolvable (caller rejects
   replace/delete as a mis-anchor, or lowers `INS.BLK.POST` to `INS.POST`).

This reproduces omp's documented semantics: a Go func → its whole
`function_declaration`; Python `@cache`+`def` anchored on the decorator →
`decorated_definition` (both); a Markdown `##` heading → the whole `section`
(heading through nested subsections up to the next same/higher heading).

Memoize by `(hash(text), len, line, path)` — the resolver may be called
repeatedly (and, later, on streaming previews). FIFO-bounded cache (omp uses 512).

`resolveBlockEdits` (`block.go`, pure) expands each `block` edit into concrete
insert/delete edits using the resolver, so `apply.go` only ever sees resolved
edits. Unresolvable replace/delete → error; unresolvable `INS.BLK.POST` → lower
to `INS.POST N` + warning (never fails the patch).

## 10. Error UX

Faithful to omp's catalog (which is a big part of why the format lands):
- **Stale tag / mismatch**: show the current file hash and ±2 lines of context
  around each anchor (`MISMATCH_CONTEXT=2`), with "STOP and re-read".
- **Out-of-range anchor**: `Line N does not exist (file has M lines)`.
- **No-op**: parsed+applied but byte-identical → explain the bug is elsewhere,
  re-read; hard error after 3 repeats.
- **Contamination**: apply_patch sentinels / `@@` headers → point to verb headers.
- **Block unresolved / single-line**: point to `SWAP N.=M` fallback.

Good errors are load-bearing: they steer the model back on track without a
human. Budget real effort here in Phase 1.

## 11. Prompts

- Port `packages/hashline/src/prompt.md` into `internal/agent/tools/` as the
  hashline edit tool description (embed or template).
- Adjust `view.md.tpl` to describe the `[path#TAG]`/`LINE:TEXT` output when in
  hashline mode (rendered from `EditMode` at construction).
- Both descriptions are only surfaced when the mode is active (string-mode
  descriptions unchanged).

## 12. Testing

Mirror the existing tools test style (`testify/require`, `t.Parallel()`,
`t.TempDir()`; mock permission/history/filetracker services already exist in
`internal/agent/tools/*_test.go`). No golden files in tools today, but the
hashline core is a great fit for table-driven golden tests.

- `internal/hashline` unit tests (pure, fast): hash stability across
  CRLF/trailing-ws; parser lenient variants + rejections; apply for every op;
  overlap/out-of-range/no-op; block expansion with a fake resolver.
- `internal/hashline/tsblock` tests: resolver spans for Go/Python/TS/Markdown
  fixtures (the Appendix A cases as a starting corpus).
- Tool-level tests: read→edit round trip, stale-tag rejection, multi-file
  atomic preflight, permission-denied path, CRLF round-trip.
- Consider porting a slice of omp's edit corpus as fixtures.

## 13. Phasing (separate PRs)

1. **Core, line ops only (shippable MVP).** ✅ Done. `internal/hashline` (hash,
   snapshot, tokenizer, parser, apply, mismatch), the Store service, Read
   hashline output, config flag + defaulting, the hashline edit tool (registered
   as `Edit`) + coordinator wiring, prompts, tests, plus a per-file diff UI.
2. **Block ops.** ✅ Done. `odvcencio/gotreesitter` (all grammars) + `tsblock`
   resolver + `SWAP.BLK`/`DEL.BLK`/`INS.BLK.POST` + resolver tests. Binary
   ~109 MB → ~141 MB.
3. **Polish.** Partially done:
   - ✅ Seen-lines provenance (reject anchors on never-displayed lines).
   - ✅ Snapshot-based 3-way-merge recovery (via `go-udiff` `Merge`; conflict →
     clean rejection, so it is strictly safe).
   - ✅ `MV` / `REM` section directives (rename / delete files).
   - ⏸ Deferred: `grep` anchor emission (ripgrep path-vs-store-key consistency
     risk + trimmed match text; Read already covers anchoring and provenance
     limits grep-edits anyway).
   - ⏸ Deferred: boundary-echo repair (low value; fragile to reconstruct SWAP
     groups from the flat post-expansion edit list; the prompt already warns).

## 14. Risks & open questions

- **Read format blast radius.** Hashline changes what every model sees on every
  read. Mitigated by the flag (opt-in) and by keeping string mode byte-identical.
- **Whole-file read for the hash.** Read is currently windowed; hashline needs a
  whole-file hash. **Decision: same 200 KB cap as string mode** — files over
  `MaxViewSize` error exactly as today; no special large-file handling.
- **No eval harness yet.** We validate qualitatively behind the flag first
  (agreed). An edit-success eval harness is a possible follow-up.
- **gotreesitter maturity.** v0.22, single maintainer; it is isolated in
  `tsblock` and only affects block ops, so the blast radius is contained. (User
  accepted this trade-off.)
- **Binary size.** +~33 MB from embedding all grammars, always linked (crush
  ~109 MB → ~140 MB). `grammar_subset*` build tags can trim this later if needed
  (verified 35 MB → 11 MB), or `tsblock` could become a build-tagged optional
  component.

### Decisions (resolved)

1. **Tool name:** reuse `Edit` in both modes (schema/behavior chosen by mode).
2. **Seen-lines provenance:** deferred to Phase 3.
3. **Large-file read:** same 200 KB cap as string mode; error above it.
4. **Grammar set:** ship all ~206 grammars (default embed), no subsetting in v1.

## Appendix A — verified tree-sitter findings

All under `CGO_ENABLED=0`, `odvcencio/gotreesitter v0.22.1`:

- Builds and runs CGO-free; `Parse` never returns an error (invalid input yields
  a tree with ERROR nodes; `Node.HasError()` detects them).
- `Point.Row` is 0-indexed; line N (1-indexed) ↔ `Row N-1`.
- Block resolver (outermost non-root named node starting on the row):
  - Go `func hello(){...}` line 3 → `function_declaration` lines 3–5.
  - Go `type T struct{...}` line 3 → `type_declaration` 3–5; `func (t T) M()`
    line 7 → `method_declaration` 7–9.
  - Python `@cache`\n`def load()` line 1 → `decorated_definition` 1–3; line 2 →
    `function_definition` 2–3.
  - Markdown `## Section A` line 5 → `section` 5–9 (heading through its body).
  - Line pointing at a `}` closer → no node starts there → nil (correct;
    single-line/no resolution is a mis-anchor).
- Grammar subsetting via build tags works: all grammars 35 MB → curated 4-grammar
  subset 11 MB, block resolution still correct.
- Markdown grammars (`markdown.bin`, `markdown_inline.bin`) are present.
