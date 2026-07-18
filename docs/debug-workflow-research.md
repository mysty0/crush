# Gated debugging workflow — research & design notes

Goal: a phased, human-gated debugging workflow that stops agents from their
three recurring failures — fabricating numbers, theorizing instead of
measuring, and blaming the environment — by forcing grounded, falsifiable
work at each stage and validating it adversarially before advancing.

## The failures we're designing against

Three symptoms, observed repeatedly, with one root cause:

1. **Fabricated fact treated as ground truth.** e.g. "debug is 5-10× slower"
   presented as measured when it was copied from a shell-script comment; no
   benchmark ever run.
2. **Theory instead of measurement (effort substitution).** Correctly
   identifies the one quantity to measure (keypress→visible latency), finds it
   costly, and emits a ranked-hypotheses list instead.
3. **Environment-blame with an incoherent mechanism.** "The one-shot open
   animation causes the sustained per-keystroke lag" — a cause whose required
   precondition (animation replays per keystroke) is directly observable-false.

Root cause (named in the literature): **premature confidence** — models
"commit to an answer early and spend the remaining tokens rationalizing it"
(Gai et al. 2026, arXiv:2605.24396; and the "preemptive answer" effect,
ACL Findings 2024). All three are the same move: emit the plausible thing
that avoids falsifiable work.

## What the research says (concrete, named methods)

### Reproduce → localize → fix pipelines (this design is the literature)

- **AutoSD — Automated Scientific Debugging** (arXiv:2304.02195). LLM runs
  Zeller's loop: Hypothesis → Prediction → Experiment → Observation →
  Conclusion, with a **real debugger executing each experiment**, and
  **cannot emit a patch until it concludes**. This is our Phase 2, verbatim.
- **Agentless** (arXiv:2407.01489). Rejects free-form agentic autonomy for a
  fixed reproduce→localize→fix pipeline; generates reproduction tests to
  filter candidate patches. Diagnoses agent failure: "limited ability to
  self-reflect," "takes misleading feedback at face value," errors compound
  over 30–40 turns.
- **RepairAgent** (arXiv:2403.17134). A literal **finite state machine**
  forcing interleaving of information-gathering / ingredient-gathering /
  validation rather than jumping to a patch.
- **AutoCodeRover / SpecRover** (arXiv:2404.05427). AST search + spectrum-based
  fault localization; reproduction tests select the final patch.
- **Delta debugging / `git bisect`** (Zeller 1999). Hypothesis-trial-result
  loop that experimentally narrows the failure to a minimal case.

### Generate-and-test > generate-and-rank

- **CodeT** (arXiv:2207.10397). Validate candidate code by **executing**
  generated tests, not by an LLM's opinion. The lesson: wherever a claim
  reduces to something executable (a failing test, a profiler number, an
  exact-match on a quoted span), prefer execution over free-form judgment.
- Generate-and-rank (an LLM scores candidates without execution) inherits
  judge biases (position, verbosity, self-enhancement) and is the weaker
  fallback (FairEval, MT-Bench paper).

### Making the adversarial judges trustworthy

- **Cite-and-verify** (RARR arXiv:2210.08726; AIS arXiv:2112.12870;
  FActScore arXiv:2305.14251). Decompose into atomic claims; require an
  **exact quoted span + source id** per claim; a **non-LLM routine
  string-checks the quote** against the source. This is the single highest-
  leverage anti-hallucination step — a judge cannot "quote" text that
  doesn't exist without failing an automatic check. (We prototyped this and
  it held: all six of a critic's citations string-matched the transcript.)
- **Panel of judges / juries — PoLL** (arXiv:2404.18796). A panel of smaller
  judges from **disjoint model families** beats one big judge and removes
  self-preference bias. → run the 3 judges on different models, in parallel,
  not seeing each other.
- **Sycophancy** (Sharma et al. arXiv:2310.13548). Judges favor the
  claimant's stated view. → **hide the author's narrative/confidence** from
  the judges; give them only the artifacts + the bare claim.
- **CriticGPT** (arXiv:2407.00215). A solo critic hallucinates bugs; a
  human+critic team catches as many while hallucinating less. → keep the
  human checkpoints; don't fully trust an automated verdict.
- Supporting: multi-agent debate (arXiv:2305.14325, 2402.06782),
  self-consistency (arXiv:2203.11171), Chain-of-Verification
  (arXiv:2309.11495).

### Human-in-the-loop gating (how frameworks pause between phases)

Every serious framework converges on the same shape: **(1) persist full
state at the pause, (2) block the state transition until an explicit
external signal, (3) route on that signal.**

- **LangGraph**: `interrupt()` + checkpointer keyed by `thread_id`; resume
  with `Command(resume=...)`; the edge to the next node can't be traversed
  without it.
- **Temporal**: workflow blocks on `wait_condition`; only a **Signal** can
  unblock; state persisted via event-history replay.
- **AWS Step Functions**: `waitForTaskToken` — a token is the sole key that
  unblocks the wait state.
- **OpenAI Agents SDK**: `needs_approval` → run pauses, returns
  `interruptions`; `RunState` serializes for out-of-process resume.
- **CrewAI**: `human_input=True` + guardrail functions with bounded retries
  as automated phase gates.

**Mapping to Crush:** we don't need any of this machinery. The **turn
boundary is the checkpoint** — the agent presents, ends its turn, and cannot
proceed until the user's next message. That message is the "signal." A small
`.crush/debug/state.json` is the "checkpointer" across turns.

## The key design unlock: enforce "no fix yet" by *execution*, not tool-ban

Banning `edit`/`write` is wrong — **every phase writes code**: Phase 1 writes
a reproduction harness/test, Phase 2 writes debug instrumentation. The gate
is fix-vs-not-fix (semantic), which a tool-name matcher can't tell apart.

Instead, use an **executable invariant** (generate-and-test):

> The reproduction test must keep **FAILING** until Phase 3.

The agent edits freely; we continuously re-run the repro test. If the bug
stops reproducing before Phase 3, it either snuck in a fix or its
"instrumentation" changed behavior — **caught by execution, not opinion, not
a tool matcher.** This is the hard "no premature fix" gate that survives the
fact that all phases legitimately write code.

## The four phases

| Phase | Deliverable | Hard gate (executable) | Adversarial gate |
|---|---|---|---|
| **1 Reproduce** | a deterministic, checked-in **failing test/harness** (not live, not ad-hoc keypress emulation) | test **fails for the right reason** on current code | 1 validator (cite-verify): does the test capture the reported symptom? |
| **2 Localize + measure** | exact `file:line` + **numbers** from instrumentation/profiler artifacts on disk (AutoSD loop) | repro test **still fails** (no-fix invariant); measurement artifacts exist on disk | **3 independent judges** (PoLL: parallel, disjoint models, author-blinded, cite-verify) must concur the cause traces to the numbers |
| **3 Propose fixes** | multiple options tied to the measured cause, each naming the test that would prove it | (proposals only; no green test required yet) | optional: sanity-check proposals address the measured cause |
| **4 Normal chat** | plain conversation | skill deactivated | — |

Discipline baked into every phase:
- **Scientific-debugging loop** (AutoSD): Hypothesis → Prediction → Experiment
  → Observation → Conclusion. A cause may only be *named* after a measurement
  or a run falsification test supports it.
- **Code-first, environment-last with an inverted evidence bar.** An
  environment cause is invalid unless its **disproof was already run**. State
  the mechanism *and its preconditions*, and check the preconditions against
  what's observable (this is where env-blame dies for free).
- **Symptom-shape match.** A one-shot cause can't explain a sustained
  symptom; a per-open cause can't explain a per-keystroke symptom.

## Enforcement inventory — hard vs soft

- **HARD (executable / structural):**
  - Human checkpoints = turn boundaries. The agent physically cannot advance
    without the user's next message.
  - Repro test fails for the right reason (Phase 1 exit) and still fails
    (Phase 2 no-fix invariant) — pass/fail is execution ground truth.
  - Measurement artifacts must exist on disk before Phase 2 can present.
- **AS-STRONG-AS-THE-JUDGES (proven robust):**
  - Validator/judges are `agent` sub-agents with cite-verify (quotes
    string-checked against artifacts), run as a parallel panel, author-blinded.
- **SOFT (instruction; backstopped by the above):**
  - The agent *choosing to stop and present* rather than barreling into the
    next phase. Backstops: the user sees a skipped gate immediately, and the
    no-fix invariant means it can't do fix-damage even if it jumps.

## Open questions for the skill design

1. How does the skill persist/advance phase state across turns, and who is
   allowed to advance it (user-only, to keep the human gate hard)?
2. How does it run the judges — parallel `agent` calls with what exact
   cite-verify contract, and what quorum (unanimous vs majority)?
3. For an input-latency bug, can Phase 1 produce a reproduction *without* live
   keypress emulation? (Instrument the real input handler to timestamp events,
   vs. a synthetic harness — the harness must be reproducible, not ad-hoc.)
4. What's the minimum that makes the no-fix invariant real (a runnable repro
   command recorded in state.json that the agent must re-run and show output
   for at each Phase 2 step)?
