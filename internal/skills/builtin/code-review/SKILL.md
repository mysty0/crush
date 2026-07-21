---
name: code-review
description: Use when the user wants a code review, architecture review, or audit of a codebase/diff/directory to find bad architecture, workarounds/hacks and technical debt, poor engineering decisions, or duplicated code — dispatches parallel subagents by review lens and synthesizes one report.
---

# Code Review

A structured way to review code by fanning out parallel read-only subagents,
each scanning through a different lens, then synthesizing their findings into
one deduplicated, severity-ranked report. Use this instead of a single
freeform review pass — a single agent trying to catch everything at once
produces lower-signal, vaguer findings than several narrowly-scoped passes.

## When to use this

The user asks to review, audit, or critique code — a diff, a directory, a
module, or the whole repo — especially when they want to find architectural
problems, hacky workarounds, bad decisions, or duplication rather than just
"any bugs."

## Step 1 — Determine scope and size

Figure out what's being reviewed: uncommitted changes (`git diff`), a PR
branch (`git diff main...HEAD`), a directory, or the whole repo. Get a rough
size (files/lines changed, or file count for a directory scan).

Scale effort to size:
- **Small** (a handful of files / a small diff): skip parallel fan-out
  entirely, review it yourself directly.
- **Medium/large**: fan out per lens (Step 2). One subagent per lens is
  usually enough.
- **Very large** (whole unfamiliar repo, dozens of files): fan out per lens
  *and* per directory/module within each lens if a single subagent's context
  would be stretched thin.

## Step 2 — Dispatch parallel review subagents (one per lens)

Launch all lens subagents in a single batch of parallel `agent` tool calls
(mode `read`) so they run concurrently in separate contexts. Do not run them
sequentially. Give each one only the scope it needs (specific paths, a diff,
or `git diff` command to run) — vague scope causes overlapping, redundant
findings between subagents.

Use these four lenses as the default set; drop or merge lenses that don't
apply to the scope (e.g. skip "architecture" for a 20-line diff):

1. **Architecture & design** — component boundaries and cohesion, circular
   or wrong-direction dependencies, god objects/modules, missing or
   over-engineered abstractions, layering violations, whether new code sits
   at the right level of abstraction, consistency with the project's
   existing architecture (read AGENTS.md / architecture docs first).

2. **Duplication & reuse** — copy-pasted logic, the same decision or rule
   expressed in more than one place ("knowledge duplication," not just
   identical text), existing helpers/utilities that should have been reused
   instead of reimplemented.

3. **Workarounds, hacks & technical debt** — code that patches around a
   limitation locally instead of fixing it upstream (especially if more than
   one caller repeats the same guard/patch), suppressed errors, disabled
   tests/lints, `TODO`/`FIXME`/`HACK`/`XXX` markers, dead or commented-out
   code, magic numbers, speculative complexity built for needs that don't
   exist yet.

4. **Decisions & conventions** — deviations from the project's stated
   conventions (AGENTS.md/CLAUDE.md/CRUSH.md), inconsistent patterns across
   similar code, poor naming, missing error handling at real boundaries
   (not speculative), anything that will clearly cause pain later even if
   it "works."

### Subagent prompt template

For each lens, give the subagent a prompt shaped like this:

```
You are reviewing [SCOPE: e.g. "the diff from `git diff main...HEAD`" or
"the internal/agent directory"] for [LENS NAME] issues only. Ignore other
concerns (bugs, security, style nitpicks, formatting) — other reviewers
cover those.

Look specifically for: [lens-specific checklist from Step 2].

Do NOT flag: pre-existing issues outside the changed/reviewed code unless
directly relevant, purely subjective style preferences, anything a linter
would already catch, or speculative "this could be a problem" issues with
no concrete evidence.

For each real issue found, report:
- file:line
- one-line summary
- why it matters (concrete consequence, not just "this is bad practice")
- suggested remedy

Only report HIGH-SIGNAL issues. If you find nothing real, say so — do not
invent issues to have something to report.
```

## Step 3 — Verify findings (medium/large scope only)

For anything beyond a small review, treat wave-1 findings as *candidates*,
not final. Dispatch one more parallel batch of subagents — one per
candidate finding or one per lens re-checking its own batch — whose only
job is to open the cited file and confirm the issue is real: right line,
right file, the described problem actually exists in the current code.
Drop anything that can't be confirmed. This is the single biggest lever
against hallucinated line numbers and false positives from Step 2.

Skip this step for small/quick reviews where the cost isn't justified.

## Step 4 — Synthesize the report

Once all subagents return, do this yourself (don't delegate synthesis):

1. Deduplicate findings that point at the same location/issue across lenses.
2. Drop anything Step 3 couldn't confirm.
3. Bucket by severity — critical/high/medium/low, or blocker/warning/notice.
4. Group the final report by lens or by severity (severity is usually more
   useful to the reader), citing `file:line` for every item.
5. Name the underlying smell where applicable (god object, circular
   dependency, shotgun surgery, feature envy, speculative generality,
   knowledge duplication) so findings are concrete and falsifiable rather
   than vague opinions.
6. If a lens found nothing, say so briefly rather than omitting it — silence
   could otherwise look like an accidental gap.

Keep the report concise: lead with critical/high items, don't pad low-signal
findings just to have more to show.
