# Bash timeouts & backgrounding — design notes

Design notes for two related improvements to the shell/bash tooling, aimed at
stopping the agent from getting stuck on long-running or non-terminating
commands (`journalctl -f`, `tail -f`, dev servers, hung builds).

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

## Research summary (best practices)

Every mature agent (Claude Code, OpenAI Codex, Gemini CLI) enforces the same
principle: **a tool call must return control to the loop in bounded time; the
tool layer — not the model — guarantees termination.**

- Claude Code: 2min default kill-timeout, 10min max; `run_in_background` →
  output to a file, read on demand; `Monitor` tool streams log lines as events;
  kills the whole process group on timeout; cleans up on session exit.
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

## Feature 2: Ctrl+B+B — force a running exec to background

### Feasibility: high — the mechanism already exists
The auto-background path (`bash.go:370`, the `<-timeout` case) does exactly what
Ctrl+B+B needs: break the wait loop with `done=false` and fall through to the
"moved to background" return. The shell is already detached, so no extra
bookkeeping.

### What's needed
1. **Tool-call-scoped signal channel**, keyed by `sessionID + callID`, modeled
   on the permission service's `pendingRequests` map (`permission.go:133`) —
   the existing UI→tool pattern. (New file, e.g.
   `internal/agent/tools/bash_background.go`.)
2. **One extra `select` case** in the bash wait loop
   (`bash.go:359-381`) listening on that channel.
3. **`Workspace.AgentBackgroundToolCall(sessionID, toolCallID)`** mirroring
   `AgentCancel` (`app_workspace.go:160`). Remote/headless mode needs a new
   proto+HTTP endpoint mirroring `CancelAgentSession`
   (`client_workspace.go:203`) — the only nontrivial extra plumbing.
4. **UI**: the active bash `ToolCallID` already streams via `BashProgressEvent`
   (`ui.go:1119`); track it, and add a Ctrl+B double-tap binding copying the
   cancel-confirm state machine (`ui.go:4000`).

Deliberately **separate** from Ctrl+C/Esc, which cancel the whole turn's
context — backgrounding must leave the turn running, so it needs its own
channel, not context cancellation.

### Risks / notes
- **Remote path**: client/server mode adds a proto+HTTP endpoint. Local TUI is
  small.
- **tmux collision**: `Ctrl+B` is the default tmux prefix. Running Crush inside
  tmux, the first `Ctrl+B` is eaten by tmux. Consider making the binding
  configurable or picking a non-colliding default.

## Suggested sequencing

1. Clarify the auto-background message to state the actual threshold (tiny, no
   behavior change).
2. Add Ctrl+B+B backgrounding (self-contained, no regression; local first,
   remote endpoint after).
3. Add the required hard-kill `timeout` last (the one behavior change with
   regression risk; needs the semantics decision + default cap).
