# Edit-tool evaluation — findings

What we learned measuring Crush's editing on the oh-my-pi
`typescript-edit-benchmark` corpus (80 single-file "find and fix a subtle bug"
fixtures, vendored at `internal/agent/testdata/edit_evals/`). This records the
results so future work doesn't re-derive them.

## Harness
Two ways to run the same fixtures, both env-gated (no API cost unless enabled):

- **Go test** — `internal/agent/edit_eval_test.go`. Drives the coder agent
  in-process against Claude Haiku 4.5 via the local Claude Code subscription
  (`~/.claude/.credentials.json`). Knobs: `CRUSH_EDIT_EVAL=1`,
  `CRUSH_EDIT_EVAL_MODES`, `CRUSH_EDIT_EVAL_ONLY/SPREAD/LIMIT/REPEAT`,
  `CRUSH_EDIT_EVAL_CONCURRENCY`, `CRUSH_EDIT_EVAL_DUMP`.
- **Script** — `scripts/edit-eval.py`. Drives the real `crush run` binary per
  fixture (more faithful; no Go test framework). Flags: `--modes`,
  `--only/--spread/--limit/--repeat`, `--concurrency`, `--dump`, `--prompt`
  (swaps the coder system prompt via `CRUSH_CODER_PROMPT_FILE`).

Scoring is exact-match of the final file vs the fixture's `expected/`.

## Reference (oh-my-pi, published, same fixtures)
oh-my-pi's own `all_models_results.json`, `--edit-variant auto` (its default is
`edit.mode: hashline`): **Claude Haiku 4.5 = 90.0%**.

## Crush results (Haiku, full 80)
| config | pass% | tool-errors/task | tokens/task |
| --- | --- | --- | --- |
| string edit | ~80% | 0.64 | 845 |
| hashline edit | ~76% | **0.35** | **801** |

- **Efficiency is a real, consistent hashline win**: ~half the tool errors and
  fewer tokens. This is the universal benefit on a strong model.
- **Pass-rate is at parity / slightly lower** on this model. Haiku is already an
  excellent string-editor (~80%), so there's little headroom for a format to
  "rescue" it — unlike the weaker models where oh-my-pi reports big jumps.

## Things we tried that did NOT move pass-rate

1. **Three hashline edit-format levers** — boundary-echo repair, fuzzy anchor
   recovery, seen-lines provenance (committed; they are correct robustness
   improvements). Controlled 5×/fixture A/B on the hardest subset: **flat,
   within noise**. Fuzzy provably can't fire on static single-shot edits; the
   failures are *finding/reasoning* failures, not edit-mechanics.

2. **Copying oh-my-pi's system prompt** (adapted to Crush's tools;
   `internal/agent/templates/coder_omp.md.tpl`, selected via
   `CRUSH_CODER_PROMPT_FILE`). A controlled A/B **misled us** on the 4 hardest
   fixtures (30%→50%), but the **full 80 regressed to 53.8%** (from ~76%).
   - Mechanism: the transcripts show the omp prompt's "grep to locate, never stop
     at the first answer, verify, be thorough" discipline makes a weak model do
     *more* — more searches, more edits — and on exact-match scoring, more
     activity means more chances to edit the **wrong occurrence** or over-edit.
     Example (`operator-swap-nullish-002`): the bug was line 176; Haiku edited a
     *different* `||`/`??` on line 127, the wrong direction, leaving the bug.
   - Root cause: oh-my-pi's prompt discipline is **coupled to oh-my-pi's tools**
     ("code intelligence → lsp", "verify → tester agent", "structural →
     ast_edit"). Stripped of those tools (which Crush lacks), the aggressive
     prose just induces thrash. **oh-my-pi's 90% is not coming from its system
     prompt.**
   - Methodological lesson: always measure prompt changes on the **full** set,
     never a hand-picked hard subset.

## Conclusion — where the gap actually is
Not the edit format, and not the system-prompt prose. The failing fixtures are
**finding/reasoning-limited**: vague prompts ("there is a subtle bug, find it")
+ subtle bugs (a dropped `!`, one of four identical lines) in ~500-line files.
The likely levers, in order:

1. **The read/find tooling.** Crush's `Read` never summarizes — a 500-line file
   is 3 raw 200-line pages. oh-my-pi's `read` returns a tree-sitter **structural
   summary** (signatures kept, bodies elided, `[…NN ln elided; re-read …]`
   footer + multi-range reads), which is purpose-built for locating a subtle bug
   with less noise. Crush now links tree-sitter (`internal/hashline/tsblock`), so
   this is portable. **Highest-leverage next step.** (Design summary below.)
2. **Structural editing** (`ast_grep`/`ast_edit`) — targets the wrong-occurrence
   failure by matching shape instead of line number. See
   [`astgrep-design.md`](./astgrep-design.md).
3. **Serving-path confound** (not portable): oh-my-pi measured Haiku via
   OpenRouter; we used the subscription with Claude Code identity injection +
   Crush's full coder prompt. Same weights, possibly different behavior.

## Appendix — summarizing-read port (design summary)
Add a structural-summary branch to `internal/agent/tools/view.go`, gated by a new
`read_summarize` config flag (default off first), reusing `tsblock`/gotreesitter:

- `Summarize(text, path) ([]Segment{Kept|Elided, start, end}, elidedLines, ok)`:
  parse; for each top-level (and shallow-nested) named node, keep its opening
  signature line(s) and closing line, elide interior body lines; merge brace
  pairs into `header { … } closer`; unknown language / syntax error / tiny file →
  `ok=false` (fall back to the raw window).
- Render kept lines verbatim; elided spans as one `N-M: …` line; append
  `[…NNln elided; re-read needed ranges, e.g. path:5-16,40-80]`.
- Coexist with hashline mode: whole-file tag stays valid; `seen` = the kept
  lines; elided ranges the model re-reads extend `seen` naturally.
- Effort ~3 days phased (summarizer → renderer → hashline-mode + footer).
- Skip when a range/offset was given, for skill files, images, or oversized
  files.
