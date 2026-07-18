# Bash timeouts & backgrounding — design notes

Design notes for improvements to the shell/bash and sub-agent tooling, aimed
at stopping the agent from getting stuck on long-running or non-terminating
operations (`journalctl -f`, `tail -f`, dev servers, hung builds, slow
sub-agent turns) and giving the user a Claude-Code-style "send this to the
background" escape hatch.

## Background: what exists today

- **Bash tool** (`internal/agent/tools/bash.go`) has **no hard kill-timeout**.
  It has `auto_background_after` (default 60s), which **detaches** a slow
  command to a background job — it never kills it. The `<-timeout` case in the
  wait loop (`bash.go:370`) breaks out and returns the command as a background
  job (`bash.go:412-422`).
- **`job_output` wait** is now bounded (default 60s, max 600s) so a
  `wait: true` on a non-terminating job can no longer block the turn forever.
- Shells are always started detached with `context.Background()`
  (`bash.go:342`) and registered in the singleton background manager, so a
  running command already survives the tool returning.
- **Sub-agents** (`agent`/`agentic_fetch` tools, and each phase dispatch
  inside a `Workflow` run) all funnel through `coordinator.runSubAgent`
  (`internal/agent/coordinator.go:1717`), which is **fully synchronous**: it
  blocks on `params.Agent.Run(ctx, ...)` until the sub-agent's whole turn
  completes. There is no way to detach one today.
- **Workflow's own top-level dispatch** (`internal/agent/workflow_tool.go:103-200`)
  is already async: it returns immediately with "Started in the
  background...", runs in a goroutine, and queues its result back into the
  parent session as a follow-up once done (`finishWorkflow`,
  `workflow_tool.go:367-413`, via `coordinator.Run(...)`). This is the
  pattern the sub-agent design below reuses.

## Research summary (best practices)

Every mature agent (Claude Code, OpenAI Codex, Gemini CLI) enforces the same
principle: **a tool call must return control to the loop in bounded time; the
tool layer — not the model — guarantees termination.**

- Claude Code: 2min default kill-timeout, 10min max; `run_in_background` →
  output to a file, read on demand; `Monitor` tool streams log lines as events;
  kills the whole process group on timeout; cleans up on session exit. Ctrl+B
  sends the currently-blocking operation (bash or a sub-task) to the
  background on demand.
- OpenAI Codex: 10s default exec; unified-exec PTY sessions with a bounded
  250ms–30s "yield time" per poll; 5min max background lifetime.
- Gemini CLI: `is_background`; `inactivityTimeout` (kill on no output).
- Consensus: separate "run to completion" (short default timeout) from
  "runs forever" (background primitive); poll, never blocking-wait; kill the
  process **group** on timeout; cap output; enforce in the tool layer and only
  *steer* with prompts (prompts change tendencies, not invariants).

## Feature 1: mandatory `timeout` the agent must always fill

### Feasibility
- In `charm.land/fantasy`, a struct field is **required by default**; the only
  thing that makes it optional is `,omitempty` in the json tag
  (`fantasy/schema/schema.go:138,146`). So a field like
  `Timeout int \`json:"timeout"\`` (no omitempty) lands in the schema
  `required` array automatically.

### The catch: "required" is not actually enforced
- Crush talks to Anthropic and OpenAI **Chat Completions with `Strict=false`**
  hardcoded (`openai/language_model.go:626`; Anthropic has no strict mode). So
  `required` is a strong *hint* the model usually honors, not a guarantee.
- A missing `timeout` unmarshals to `0`, indistinguishable from an explicit
  `0`. **So the executor must default/clamp regardless** (reject `<= 0`, apply
  a default, cap at a max). The required flag improves model behavior; the real
  safety lives in the tool layer.

### Open decision: semantics of `timeout`
- **(a) Hard kill** after N seconds (kill the process group) — matches best
  practice, but is a **behavior change**: long builds/tests that currently
  survive via auto-background would start getting killed. Needs a generous
  default cap (cf. Claude's 2min/10min).
- **(b) Keep detach semantics** and just require the model to name the
  threshold — safer, but it is not really a "timeout," just the existing
  auto-background with a required value.

### Recommended model
Two distinct knobs the model reasons about:
- `timeout` (required, hard **kill** cap): "this should finish within N
  seconds; kill it if not." Gives a genuine "timed out and was killed after
  Ns" state to display.
- `run_in_background` (optional): for things that never terminate (servers,
  watchers); timeout does not apply.

Supporting changes: add matching field to `BashPermissionsParams`
(`bash.go:32-38`), wire the value into the wait loop, update `bash.md.tpl` and
`bash_test.go`.

## Feature 2: Ctrl+B — force whatever's currently blocking to the background

Claude Code's Ctrl+B backgrounds *whatever the agent is currently waiting
on* — a long bash command or a slow sub-task — without canceling the turn.
Crush should offer the same for its two long-running, synchronous
operations: the `bash` tool's foreground wait, and `runSubAgent`'s blocking
`Agent.Run` call (which underlies the `agent`, `agentic_fetch`, and
per-phase `Workflow` dispatches).

Deliberately **separate** from Ctrl+C/Esc, which cancel the whole turn's
context — backgrounding must leave the operation running, so it needs its
own signal, not context cancellation.

### 2.1 Shared primitive: a per-session background-request registry

New file `internal/agent/tools/background.go`, a package-level singleton in
the same style as `bash_progress.go`'s `bashProgressBroker`. Keyed by
`(sessionID, toolCallID)` so multiple concurrent blocking operations in one
session (e.g. parallel `agent()` calls in a single step) can each be
registered and fired independently, while a session-wide fire (Ctrl+B)
signals all of them at once:

```go
// RegisterBackground registers a blocking tool call as backgroundable and
// returns a trigger that closes its channel when BackgroundNow fires for
// this session, plus an unregister func to call once the operation
// finishes on its own.
func RegisterBackground(sessionID, toolCallID string) (trigger *BackgroundTrigger, unregister func())

// BackgroundNow signals every operation currently registered for
// sessionID and returns how many were fired (0 if nothing was blocking).
func BackgroundNow(sessionID string) (fired int)
```

`BackgroundTrigger.C()` returns a channel that closes on fire, usable
directly in a `select`.

### 2.2 Bash integration (small change)

Register before the existing wait loop; add one more `select` case that
does exactly what the existing `<-timeout` case already does:

```go
trigger, unregister := tools.RegisterBackground(sessionID, call.ID)
defer unregister()
...
case <-trigger.C():
    stdout, stderr, done, execErr = bgShell.GetOutput()
    break waitLoop   // falls into the existing "!done -> background response" branch
```

The auto-background path (`bash.go:370`, the `<-timeout` case) already does
exactly what's needed here — the shell is already started detached
(`bash.go:342`), so no extra bookkeeping is required beyond the new
`select` case. Only cosmetic change beyond that: distinguish "you asked to
background this" from "it timed out" in the returned message (currently
`bash.go:421` always says "taking longer than expected").

### 2.3 Sub-agent integration (the real work)

`runSubAgent` (`internal/agent/coordinator.go:1717`) currently blocks
inline on `params.Agent.Run(ctx, ...)`. Restructure it to run the sub-agent
in a goroutine and `select` on three outcomes:

```go
bgCtx, bgCancel := context.WithCancel(context.WithoutCancel(ctx)) // detached from the parent turn up front, like Workflow already does
trigger, unregister := tools.RegisterBackground(params.SessionID, params.ToolCallID)
defer unregister()

resultCh := make(chan outcome, 1)
go func() { resultCh <- runWithRetry(bgCtx) }()

select {
case out := <-resultCh:
    bgCancel()
    return completeSync(out)              // today's existing formatting logic, unchanged

case <-trigger.C():
    go func() {
        out := <-resultCh
        completeBackgrounded(params, out) // queues the result via coordinator.Run(...), exactly like finishWorkflow does today
    }()
    return fantasy.NewTextResponse(fmt.Sprintf(
        "Moved the sub-agent to the background (session %s) — I'll follow up when it's done. Check progress with AgentProgress(session_id=%q).",
        subSession.ID, subSession.ID)), nil

case <-ctx.Done():
    bgCancel()      // parent turn was canceled (Esc) before backgrounding — propagate, don't orphan it
    <-resultCh
    return fantasy.ToolResponse{}, ctx.Err()
}
```

Key design point: using `context.WithoutCancel(ctx)` unconditionally means a
parent-turn cancel (Esc) would stop propagating into the sub-agent through
Go's context tree — so the `case <-ctx.Done()` branch explicitly
re-propagates that cancellation via `bgCancel()`, preserving today's "Esc
kills everything" behavior for the common case. Only when Ctrl+B fires does
the sub-agent truly detach and outlive the turn.

No changes needed to existing sub-agent machinery:
- `AgentCancelSubAgent` / `AgentSendToSubAgent` already operate on the
  sub-session's own `activeRequests` entry, keyed by session ID and set
  inside `Agent.Run` itself — so canceling or steering a backgrounded
  sub-agent later still works exactly as today.
- `AgentList` / `AgentProgress` already read directly from
  `subAgentRegistry` plus persisted message history, unaffected by whether
  the dispatching tool call has already returned.

This covers the `agent` and `agentic_fetch` tools for free (both funnel
through `runSubAgent`). A `Workflow` run's own top-level dispatch doesn't
need this (already async); its *internal* per-phase sub-agent calls also go
through `runSubAgent` and would technically support backgrounding too, but
since that path already runs inside an already-detached workflow goroutine,
Ctrl+B would only reach it if the user has drilled into that specific
workflow-session view — harmless either way.

### 2.4 Plumbing to the UI

Follows the exact existing `AgentCancel` chain
(`internal/workspace/workspace.go:93`) so it works in both local and
client/server mode:

1. `Workspace.AgentBackgroundNow(sessionID string) int` — new interface
   method.
2. `AppWorkspace.AgentBackgroundNow` → calls `tools.BackgroundNow(sessionID)`
   directly (in-process, local mode).
3. `ClientWorkspace.AgentBackgroundNow` → new RPC mirroring
   `CancelAgentSession` (`client_workspace.go:209`); needs a matching
   proto + HTTP endpoint on the server side. This is the only nontrivial
   extra plumbing (remote path).
4. **Keybinding**: add to `internal/ui/model/keys.go`, handled next to
   `Chat.Cancel` in `ui.go:2549` — same session-scoping logic (main session
   vs. `m.subAgentSessionID` if the user has drilled into a sub-agent view).
5. **Feedback**: `util.ReportInfo(...)` showing how many operations were
   backgrounded, or a "nothing to background" info message if the count is
   0 (mirrors Claude Code's no-op-when-idle behavior).

### Risks / notes

- **tmux collision**: `Ctrl+B` is the default tmux prefix. Running Crush
  inside tmux, the first `Ctrl+B` is eaten by tmux unless the user has
  remapped their prefix. Needs either a configurable binding or an
  alternate default that doesn't collide (e.g. requiring the double-tap
  `Ctrl+B` `Ctrl+B` inside a short window, similar to the existing
  Esc-Esc-to-cancel confirmation state machine in `ui.go:4220`) so a
  single accidental Ctrl+B (swallowed by tmux) can't half-trigger anything.
- **Remote path**: client/server mode adds one proto+HTTP endpoint; local
  TUI wiring is small by comparison.

### Open questions

1. Bash response text: OK to split "auto-backgrounded after timeout" vs.
   "you asked to background this" wording?
2. Should the Ctrl+B feedback message show counts per kind (e.g.
   "backgrounded 1 bash command, 2 sub-agents") or just a generic total?
3. Single-tap vs. double-tap Ctrl+B, given the tmux-prefix collision above?

## Suggested sequencing

1. Clarify the auto-background message to state the actual threshold (tiny,
   no behavior change).
2. Build the shared `internal/agent/tools/background.go` registry.
3. Wire it into `bash.go` (self-contained, no regression).
4. Wire it into `runSubAgent` (covers `agent`/`agentic_fetch`/`Workflow`
   phase dispatches).
5. Plumb `Workspace.AgentBackgroundNow` (local first, remote endpoint
   after) and add the Ctrl+B keybinding.
6. Add the required hard-kill `timeout` last (the one behavior change with
   regression risk; needs the semantics decision + default cap from
   Feature 1).
