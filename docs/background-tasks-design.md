# Unified background-task abstraction — design & Option B plan

## Problem

Crush has four independent trackers for "a thing running in the background,"
each plumbed separately to the UI. The bottom picker (`agentListEntries`,
`internal/ui/model/subagent.go:409`) stitches three of them together ad hoc,
and misses the fourth entirely:

| Task type | Registry / source | In bottom picker? | Opened via |
|---|---|---|---|
| Sub-agents (`agent`/`agentic_fetch`) | `subAgentRegistry` — **but picker reads chat items, not this registry** | foreground only | `enterSubAgentView` |
| Background workflows | `workflowRegistry` → `AgentRunningWorkflows()` | yes | `enterWorkflowView` |
| Scheduled tasks (cron/wakeup) | `scheduleRegistry` | yes | cancel dialog |
| Ctrl+B backgrounded blocking ops | `tools.backgroundRegistry` (fires triggers) | no | — |
| Background bash jobs | `shell.BackgroundShellManager` | **no** | agent-only `job_output`/`job_kill` |

Two concrete defects fall out of this fragmentation:

1. **Sub-agent picker rows are derived from chat items** (`subAgentEntryFor`,
   subagent.go:333, filters on `Finished()`), while workflows/schedules come
   from registries. Backgrounding a sub-agent returns a tool result → its chat
   item reads `Finished()` → it drops off the list even though its session is
   still running (tracked in `subAgentRegistry`). This is the "can't select a
   backgrounded sub-agent" bug.
2. **Background bash jobs have no UI presence at all** — reachable only by the
   model via `job_output`/`job_kill`.

The three status structs already share a skeleton — `SubAgentStatus`,
`WorkflowStatus`, `ScheduledTaskStatus` all carry a handle, an owning session,
a label, a state, a `StartedAt`, and implicit open + cancel actions. All four
registries share the same lifecycle: `register → get/list → finish|stop →
remove (after linger)`, plus per-type `mutate` for progress. The domain is one
concept ("a background task") wearing four coats.

## The load-bearing constraint

The **`Open` action is inherently UI-bound**: it maps a task to a *view* or
*dialog* (`enterSubAgentView` / `enterWorkflowView` / schedule cancel dialog /
a not-yet-existing bash-tail view). It cannot live in `internal/agent` or
`internal/shell`, which know nothing about the UI.

Therefore a single pure `BackgroundTask` interface with an `Open()` method is a
fantasy — the open/cancel polymorphism must sit at the UI edge keyed by `Kind`
no matter what. And the per-type state (workflow phases, schedule reschedule
channels, bash stdout/stderr) means one flat mega-record would be lossy.

**So Option B is NOT "one registry replaces four."** It is:

> a **facade** + a **projection** + a **single event stream**, with the four
> specialized registries kept as the per-type cores behind the facade.

This gives real unification (one read path, one event stream, one UI list,
trivial to add task types) without a lossy mega-struct, and crosses the
`agent`↔`shell` boundary via a thin adapter.

## Registry comparison (what the facade must absorb)

| | key | filter scope | progress detail | cancel | package |
|---|---|---|---|---|---|
| sub-agents | SessionID | parent session | msg/tool counts (live pull) | trigger / ctx | `agent` |
| workflows | SessionID | **global (no filter)** | phases + agents | `context.CancelFunc` | `agent` |
| schedules | TaskID | origin session | nextFire / runCount | `stop()` | `agent` |
| bash jobs | shell ID | none (no session) | stdout/stderr/done | `Kill(ctx)` | `shell` |

## Option B — implementation outline

### 1. Domain projection — `internal/agent/task.go`

```go
type TaskKind  string // "subagent" | "agentic_fetch" | "workflow" | "schedule" | "bash"
type TaskState string // "running" | "done" | "failed" | "stopped"

// TaskRef is a stable, kind-tagged handle usable across every registry.
type TaskRef struct { Kind TaskKind; ID string }

type TaskStatus struct {
    Ref          TaskRef
    OwnerSession string      // parent/origin session ("" for bash)
    Label        string
    State        TaskState
    StartedAt    time.Time
    FinishedAt   time.Time   // zero while running
    Detail       any         // *SubAgentStatus | *WorkflowStatus | … full rich struct
}
```

`Detail` preserves the full type-specific struct so the view layer loses
nothing when it opens a task.

### 2. `TaskManager` facade — `internal/agent`

Owns the three agent registries and holds a handle to
`shell.BackgroundShellManager` via a thin **adapter** (maps `BackgroundShell →
TaskStatus`, crossing the `agent`↔`shell` boundary cleanly). Exposes exactly:

```go
Tasks(sessionID string) []TaskStatus   // merged, deduped by Ref, filtered, sorted
Cancel(ref TaskRef) error              // dispatches to the owning registry
Get(ref TaskRef) (TaskStatus, bool)
```

Each registry gains a small `asTaskStatus()` mapper. The `TaskManager` is the
single place the coordinator/workspace ask for "running things."

### 3. Unified event stream — `TaskStatusEvent` broker

Every registry publishes on `register` / `finish` / `mutate`. Replaces the
current mix (`WorkflowStatusEvent` for workflows; sub-agents piggy-backing on
message events; schedules ad hoc). The picker subscribes **once** and
re-renders on any task change. This is a genuine unification win — refresh is
inconsistent per type today.

### 4. Workspace seam

Replace the three separate methods with, mirroring the existing
`AgentRunningWorkflows` plumbing (interface + AppWorkspace + ClientWorkspace +
server/client proto):

```go
AgentTasks(sessionID string) []agent.TaskStatus
AgentCancelTask(ref agent.TaskRef)
```

### 5. UI edge — `agentListEntries()` collapses

```
for t := range Workspace.AgentTasks(session):
    row(t)
```

The `Kind → open/cancel` `switch` stays but becomes the ONLY one, in one place:

```
subagent / agentic_fetch → enterSubAgentView(t)
workflow                 → enterWorkflowView(t)
schedule                 → scheduleCancelDialog(t)
bash                     → enterBashOutputView(t)   // NEW small tail view
```

This deletes `subAgentEntryFor`'s chat-scanning entirely — **sub-agents now
come from the registry, which fixes the backgrounded-sub-agent bug for free**
(the reason this whole investigation started).

### 6. The one genuinely new surface — bash-tail view

`enterBashOutputView` streams `BackgroundShell.GetOutput()` (stdout/stderr/done,
`internal/shell/background.go:238`). Small; the sub-agent/workflow views are the
precedent.

## Phasing (incremental, each step ships independently)

1. **Projection + manager over the existing registries; sub-agents sourced
   from `subAgentRegistry`.** Ships: unified picker for the 3 agent kinds +
   **backgrounded-sub-agent bug fixed**. (This is the "Option A" foundation.)
2. **Bash adapter + bash-tail view.** Background bash jobs become
   visible/openable.
3. **Unified `TaskStatusEvent` stream.** Replace ad-hoc refresh; delete the
   special-case wiring.
4. *(Optional)* Collapse duplicated `register/finish/linger` boilerplate into a
   shared `taskLifecycle` mixin the specialized registries embed.

## Decisions to lock before coding

- **Filter scope.** Workflows list globally today; sub-agents/schedules filter
  by owning session. Unify to one policy — proposed: **per current session's
  task tree** (what you launched here), dropping the workflow global-ness as an
  inconsistency.
- **Finished tasks in the list.** Running-only (vanish on completion; result is
  already folded into chat) vs. a brief ✓/✗ linger like workflows show.
  Proposed: running-only for sub-agents/bash; keep the terminal marker for
  workflows/schedules (which have a meaningful terminal state).
- **Bash "owner" session.** Background bash has no session today
  (`BackgroundShellManager.Start` doesn't record one). Decide whether a bash
  job shows in every session's list or only the launching one — the latter
  needs threading the launching `sessionID` into `Start`.

## Blast radius

- Phase 1 mirrors the existing, working `AgentRunningWorkflows` plumbing: ~1
  manager type, small per-registry mappers, the workspace mirror, and the
  `agentListEntries` rewrite. Low risk; deletes more UI code than it adds.
- Phase 2 adds one adapter (across the shell boundary) + one small view.
- Phase 3 is a net simplification (one broker replaces several refresh paths).
