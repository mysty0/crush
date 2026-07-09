# Pure-Go structural search & edit (ast-grep in Go) — design

Status: **proposal / design doc** (no code yet). Goal: give Crush `ast_grep`
(structural search) and `ast_edit` (structural rewrite) tools that reproduce the
useful subset of [ast-grep](https://ast-grep.github.io)'s behavior, implemented
**in pure Go on the tree-sitter runtime Crush already links**
(`github.com/odvcencio/gotreesitter`), with **no CGO, no Rust, no external
binary**. It is a from-scratch reimplementation of ast-grep's matching algorithm
(ast-grep is MIT; we port the algorithm, not the code).

## 1. Why

Line-anchored editing (hashline) fails on files with repeated/near-identical
lines: the model picks the wrong line number and the edit lands silently on the
wrong occurrence (measured, see the edit-eval work). A **structural** edit sidesteps
line numbers entirely: "change `formatIsoDate($A) || $B` to `formatIsoDate($A) ??
$B`" matches by *shape*, so the model disambiguates by writing a more specific
pattern, not by counting lines. This is the one omp capability that plausibly
attacks that failure class, and it's a broadly useful search/codemod tool
regardless of the benchmark.

### Non-goals (v1)
- Full ast-grep parity: strictness modes other than `smart`, YAML rules,
  relational constraints (`inside`/`has`/`follows`/`precedes`), `selector`,
  `transform`, `constraints`, multi-line indent re-flow.
- Cross-language single call. v1 infers one language per file by extension.
- The two-phase hidden `resolve` apply handshake omp uses — Crush's permission
  prompt already *is* the human-in-the-loop preview.

## 2. What ast-grep actually does (the algorithm we port)

Verified against ast-grep's docs + `ast-grep-core` source. Full spec lives in
this doc's Appendix; the essentials:

1. **Pattern = code + metavars.** The pattern string (`$A || $B`, `foo($A)`,
   `if ($C) $BODY`) is parsed **with the same tree-sitter grammar as the target**
   into a "pattern tree." Metavars parse as ordinary `identifier` nodes whose
   text matches `^\$[A-Z_][A-Z0-9_]*$` (single) or `$$$NAME`/`$$$` (variadic).
2. **Unwrap to the significant node.** Descend from the parse root while a node
   has exactly one child; stop at the first leaf or first node with ≥2 children.
   `$A || $B` → `binary_expression`; `foo($A)` → `call_expression`; `if ($C)
   $BODY` → `if_statement`.
3. **Match** the pattern node against every node of the target tree (pre-order
   DFS), recursively: kinds must match (compared by numeric `kind_id`); a
   metavar node binds the target node it faces; leaves compare text.
4. **`smart` strictness** (the default, and all we implement in v1): every
   *pattern* node must be matched, but **unnamed** nodes in the *target* may be
   skipped during child alignment, and leftover **trailing** target nodes are
   ignored. This is what lets `function $A(){}` match `async function bar(){}`
   and tolerate trailing commas.
5. **Rewrite** parses the rewrite template with the same grammar, DFS-substitutes
   its metavar leaf nodes with the captured target nodes' **source bytes**, and
   splices the result over the matched root node's `[StartByte, EndByte)`.

### Verified feasibility on gotreesitter
A ~12-line Go matcher already runs (CGO-free) and finds all 3 structural matches
of `$A || $B` in a sample, binding whole subtrees to `$A`/`$B`. Every primitive
we need exists: `Symbol()` (fast uint16 kind id), `IsNamed()`,
`NamedChild(i)`/`NamedChildCount()`, `Child(i)`/`ChildCount()`, `StartByte()`/
`EndByte()`, `Text(src)`, `HasError()`, `ChildByFieldName`, a tree-sitter
**Query engine** (`NewQuery`/`QueryCursor`) for candidate anchoring, and
byte-accurate splicing (offsets are UTF-8 byte offsets). Nothing required is
missing from the library; the ast-grep semantics layer is entirely ours.

## 3. Architecture

```
internal/astgrep/                     pure-Go ast-grep engine (no I/O, isolated)
  pattern.go     Pattern compile: parse snippet, unwrap, build PatternNode tree,
                 detect metavars (extract_meta_var).
  metavar.go     Metavar detection + MetaVar types (Capture/Dropped/Multi/MultiCapture).
  match.go       matchNode (smart strictness), child alignment (skip unnamed
                 target nodes + trailing), metavar binding, repeated-var equality,
                 $$$ variadic (Phase 2). findAll traversal.
  rewrite.go     Rewrite template compile + gen replacement (byte splice) +
                 overlap check + apply end-to-start.
  lang.go        Language resolution by path/name via gotreesitter grammars
                 (reuse internal/hashline/tsblock's detection where possible).
  engine.go      Search(pattern, lang, src) []Match ; Rewrite(ops, lang, src)
                 (newSrc, []Replacement, error). Pure funcs, byte-oriented.

internal/agent/tools/
  ast_grep.go    ast_grep tool (search across files/globs) + ast_grep.md
  ast_edit.go    ast_edit tool (preview+apply through permission/history/LSP) + ast_edit.md
```

`internal/astgrep` shares tree-sitter with `internal/hashline/tsblock` — both
wrap `gotreesitter`. To keep one tree-sitter import site, put language detection
+ parser construction in a small shared spot (either reuse `tsblock` or a tiny
`internal/tsparse` both import). The engine package is otherwise pure and unit
-testable with in-memory strings.

## 4. Data model

```go
// A compiled pattern.
type Pattern struct {
    Root       *patternNode
    Lang       *gts.Language
    Strictness Strictness // v1: only Smart
}

type patternNode struct {
    kind     uint16        // gotreesitter Symbol() of the node
    isNamed  bool
    text     string        // for terminals (leaf)
    metavar  *MetaVar      // non-nil => this node is a metavar placeholder
    children []*patternNode
}

type MetaVarKind int
const ( MVCapture MetaVarKind = iota; MVDropped; MVMulti; MVMultiCapture )
type MetaVar struct { Kind MetaVarKind; Name string; Named bool }

// A match against a target file.
type Match struct {
    Start, End int              // byte range of the matched root node
    StartLine, EndLine int      // 1-based, for rendering
    Env  map[string]Capture     // metavar name -> captured node span
}
type Capture struct { Start, End int; Nodes []nodeSpan } // Nodes for $$$

// Rewrite op + result.
type RewriteOp struct { Pattern, Rewrite string } // empty Rewrite deletes
type Replacement struct { Start, End int; Text string }
```

## 5. Matching algorithm (v1, smart only)

`matchNode(pat *patternNode, cand *gts.Node, env, src) result`:
- **metavar**: `MVCapture{Named:true}` requires `cand.IsNamed()`; on first bind
  store `cand`'s span; on re-bind require `nodesMatchExactly(prev, cand)` (identity
  → named-leaf text equality → recursive kind+children equality — the gh#1087
  shortcut). `MVDropped` matches one node, no capture. (`$$$` handled at
  child-list level, Phase 2.)
- **terminal (leaf)**: `kindsMatch(pat.kind, cand.Symbol()) && (!pat.isNamed ||
  bytesEqual(pat.text, cand.Text(src)))`.
- **internal**: `kindsMatch(pat.kind, cand.Symbol())` then align children.

`kindsMatch(a, b) = a == b || a == errorSymbol(65535)`.

**Child alignment (the "smart" core)** — iterate pattern children `P` and
candidate children `C` (use `Child(i)`/`ChildCount()` so unnamed nodes are
visible, since smart reasons about them):
1. Try `matchNode(P.peek, C.peek)`.
2. On match → advance both.
3. On mismatch, if `C.peek` is **unnamed** → skip candidate (advance C only), retry.
4. If candidates run out while pattern children remain → fail (unless the
   remainder are all `$$$`/skippable — Phase 2).
5. When pattern children exhausted → **accept** any remaining trailing candidate
   children (smart's skip-trailing).

`findAll(pattern, root, src)`: pre-order DFS; optionally pre-filter candidates by
the pattern root's kind using a tree-sitter Query `(<kind>) @m` for speed; fresh
`env` per candidate; yield matches. (Default = find nested matches too, like
ast-grep.)

## 6. Rewrite

`rewrite(op, lang, src) []Replacement`:
1. Parse `op.Rewrite` as a template with `lang`; if it fails to parse, error.
2. DFS the template tree; for each **named leaf** whose text is a metavar, emit
   the captured node's source bytes from `env`; for `$$$NAME` emit the contiguous
   source slice `[nodes[0].start, nodes[last].end)` (preserves original
   separators). Non-metavar template bytes are copied verbatim.
3. The replacement's target range is the matched root node's `[Start, End)`.
4. Empty `op.Rewrite` → deletion (replace range with "").
5. **Apply per file end-to-start**, with an **overlap check** (abort if two
   replacements overlap — "refine pattern to avoid ambiguous edits").

v1 skips ast-grep's multi-line indentation re-flow (the common single-line
targets need none). Note it as a known gap; add later if multi-line rewrites
misindent.

## 7. Tools

### `ast_grep` (read-only)
```go
type AstGrepParams struct {
    Pattern  string `json:"pattern"`             // e.g. "$A || $B"
    Path     string `json:"path,omitempty"`      // file/dir/glob; default workingDir
    Language string `json:"language,omitempty"`  // inferred from extension if empty
}
```
Modeled on `grep.go`: walk files (reuse the workspace walker/glob + gitignore that
grep uses), parse+`findAll` per file, render grouped by file:
```
Found N matches
path/to/file.ts:
  Line L: <matched first line, truncated>
    meta: A=<capture text>, B=<capture text>
```
Cap ~100 matches. Read-only; add to `resolveReadOnlyTools`.

### `ast_edit` (mutating)
```go
type AstEditParams struct {
    FilePath string      `json:"file_path"` // v1: single file
    Ops      []AstEditOp `json:"ops"`       // [{pattern, rewrite}]
    Language string      `json:"language,omitempty"`
}
```
Flow (reuse the hashline/string edit plumbing exactly):
1. Read-before-edit + mod-time/staleness check (as `edit.go`).
2. `oldLF, isCrlf := ToUnixLineEndings(content)`. For each op: `engine.Rewrite`
   → accumulate replacements; overlap-abort; produce `newLF`. No match → "No
   replacements made."
3. Diff old→new; **permission request** with `EditPermissionsParams{FilePath,
   OldContent, NewContent}` (so the existing diff UI renders the preview — this
   *is* omp's "preview").
4. On grant: restore CRLF, `os.WriteFile`, history `CreateVersion`,
   `filetracker.RecordRead`, `notifyLSPs`, `getDiagnostics`.
5. **Hashline-mode aware**: if `EditMode==hashline`, after writing mint a fresh
   snapshot tag via `store.Record(session, absPath, newLF, nil)` and return the
   `[path#TAG]` header + `HashlineEditResponseMetadata` so the diff renderer and
   the next edit's anchors stay consistent. (Requires passing `store` + editMode
   into the tool, like the hashline edit tool.)

Worked example — 4 identical `if (x)` lines, target only the one in `formatX`:
the model can't do this with a specific-enough single pattern alone if the
bodies are identical, so v1's honest answer is: **make the pattern carry
distinguishing structure** (e.g. `formatIsoDate($A) || $B` rather than `$A ||
$B`). Targeting by *enclosing function* needs ast-grep's `inside` relational
constraint — deferred to Phase 3 (it's the highest-value follow-up).

## 8. Config / gating
- New `Options` field or reuse `Tools`: register `ast_grep`/`ast_edit` only when
  enabled (default **off** behind a flag first; promote after validation).
- `allToolNames()` gains `"ast_grep"`, `"ast_edit"`; `ast_grep` also into the
  read-only subagent set.
- Coordinator wiring near `NewGrepTool`; `ast_edit` needs the edit-tool deps
  (`lspManager, permissions, history, filetracker`, plus `snapshots`+editMode).

## 9. Testing
- `internal/astgrep` unit tests (pure, fast): pattern compile + unwrap for
  Go/TS/Python; metavar detection table (`$A`, `$_`, `$$$X`, rejects `$abc`,
  `$1`); smart matching (skips unnamed `async`, tolerates trailing comma);
  repeated-var equality; rewrite byte-splice + capture substitution; overlap
  abort; delete op. Use small in-memory sources.
- Cross-language fixtures (Go, TS, Python, Rust) for the four target patterns.
- Tool-level tests reuse the existing mock permission/history/filetracker
  services (as `hashline_edit_test.go` does), incl. a hashline-mode round trip
  and a CRLF round trip.
- Optional: wire `ast_edit` into the edit-eval harness as a third mode and
  measure whether structural edits beat line-anchored on the duplicate-line
  fixtures (the actual hypothesis).

## 10. Phasing
1. **Engine core + `ast_grep`** (~3-4 days). `internal/astgrep` (pattern, metavar,
   smart match for fixed-arity single-node patterns, findAll, rewrite for
   single-line) + the read-only `ast_grep` tool + tests. No `$$$`, smart only.
   Ships structural *search* — immediately useful, low blast radius.
2. **`ast_edit` + `$$$` variadic** (~3-4 days). The mutating tool through the full
   edit plumbing (permission/history/LSP/hashline), plus variadic metavars
   (`foo($$$ARGS)`) with the clone-env greedy lookahead (the fiddly part — see
   Appendix "genuinely hard" items). Multi-op per file + overlap.
3. **`inside`/relational scoping + indent re-flow** (~4-6 days, optional). The
   `inside: <pattern>` constraint (target one occurrence by its enclosing
   construct — the real disambiguation win) and multi-line indentation handling.
   Bigger; do only if Phase 1-2 prove the value.

## 11. Risks
- **Faithful smart-matching is subtle.** The child-alignment skip rules and the
  `does_node_match_exactly` leaf shortcut must be reproduced or repeated-metavar
  and unnamed-node behavior drift. Mitigate with a fixture corpus lifted from
  ast-grep's own tests.
- **`$$$` lookahead correctness** (Phase 2): must probe the next pattern node
  against a **cloned** env so failed probes don't leak bindings, and trim
  trailing skipped separators from the capture. Known-hard; test heavily.
- **Grammar-version coupling.** Kinds are numeric `kind_id`s specific to the
  compiled grammar; pattern and target are parsed with the *same* gotreesitter
  grammar, so this is inherently consistent — but pin the gotreesitter version.
- **Pattern parse errors.** A malformed/partial pattern yields ERROR nodes;
  detect `HasError()` on the unwrapped pattern and return a clear "invalid
  pattern" message (optionally retry wrapping the snippet in statement/expression
  context).
- **No indent re-flow in v1** → multi-line rewrites may misindent; documented,
  Phase 3.

## Appendix — ast-grep algorithm reference (for implementers)

Metavar detection `extractMetaVar(text)`:
- `text == "$$$"` → Multi. `"$$$"+X` with X all `[A-Z0-9_]`, X[0] not digit →
  MultiCapture(X) (or Multi if X starts `_`).
- else must start `$`; strip; optional second `$` → `Named=false`; remainder
  first char `[A-Z_]`, rest `[A-Z0-9_]` → Capture(name) (Dropped if starts `_`).
  Lowercase/digit-first/empty → not a metavar.

`smart` skip matrix (leaf pattern vs candidate, when not directly equal):
skip-goal = **no** (pattern always required); skip-candidate = **yes iff
candidate unnamed**; comments not skipped; trailing candidates skippable; when
candidates exhausted with goal remaining, only `$$$` skippable.

Rewrite substitution operates on the **parsed template's named-leaf metavar
nodes**, emitting captured **source bytes** (MultiCapture = one contiguous slice,
original separators preserved); everything else copied verbatim from the template
source; replaced range = matched root node byte span.

Genuinely hard to replicate exactly (flagged): pattern parser error-recovery
(version-dependent — don't bit-match); the named-leaf equality shortcut
(gh#1087); `$$$` greedy end-detection with env cloning (gh#2670); the full
per-strictness skip matrices (only `smart` needed initially).
