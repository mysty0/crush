# Crush vs. Claude Code — Feature Comparison

This document compares Crush against the reference `claude-code` implementation
to track feature parity and prioritize work. It is a living document; update it
as features land.

> Note on "deep research": this is **not** a standalone feature in claude-code.
> It is the Explore sub-agent (`AgentTool` with `subagent_type=explore`). Crush
> already has the sub-agent plumbing, so this reduces to "add an explore agent
> preset" (see Sub-agents below).

## Legend

- ✅ Present / at parity
- ⚠️ Partial — exists but limited compared to claude-code
- ❌ Missing

## Major subsystems

| Feature | Crush | claude-code | Notes |
|---|---|---|---|
| IDE Bridge (VS Code/JetBrains backend, JWT auth, diff display) | ❌ | ✅ `src/bridge/` | No IDE backend mode. |
| Multi-agent teams / Coordinator (parallel teammates, inter-agent messaging) | ❌ | ✅ `src/coordinator/` | Crush has single-level sub-agent delegation only — no teams, no `SendMessage`. |
| Background Task system (managed task objects, remote agent tasks, DreamTask) | ⚠️ | ✅ `src/tasks/` | Crush has background bash jobs (`job_output`/`job_kill`) but no managed task objects or remote agents. |
| Plan Mode (read-only planning, then execute) | ✅ | ✅ `EnterPlanModeTool` | Crush gates mutating tools (Bash, Edit, MultiEdit, Write, download) on a runtime plan-mode flag; toggle via the `Toggle Plan Mode` command. |
| Git Worktree isolation (`Enter/ExitWorktree`) | ⚠️ | ✅ | Crush resolves worktree roots but cannot create/isolate work in a worktree. |
| Voice (STT streaming, voice keyterms) | ❌ | ✅ `src/voice/` | |
| Plugin system (installable marketplace plugins) | ❌ | ✅ `src/plugins/` | Crush uses MCP + skills instead. |
| Remote / Mobile / Desktop (remote agents, mobile, teleport, cron, remote triggers) | ❌ | ✅ | |
| Memory auto-extraction & team sync | ⚠️ | ✅ `extractMemories`, `teamMemorySync` | Crush has static context files (AGENTS.md/CRUSH.md/CLAUDE.md) only — no auto-extraction or sync. |

## Sub-agents

Crush **has** sub-agents, but in a basic form.

| Capability | Crush | claude-code |
|---|---|---|
| Spawn a sub-agent with a prompt | ✅ `agent` tool | ✅ `AgentTool` |
| Parallel sub-agent calls | ✅ `fantasy.NewParallelAgentTool` | ✅ |
| Per-sub-agent session + cost rollup | ✅ `runSubAgent` → `updateParentSessionCost` | ✅ |
| Dedicated web-research sub-agent | ✅ `agentic_fetch` | ✅ |
| Multiple/selectable sub-agent **types** (`subagent_type`) | ❌ (2 hardcoded: `task`, `agentic_fetch`) | ✅ explore, bughunter, custom |
| User-defined custom agents | ❌ | ✅ markdown agent definitions |
| Inter-agent messaging | ❌ | ✅ `SendMessageTool` |
| Agent teams | ❌ | ✅ `TeamCreate/Delete` |

## Tools

Missing tools in Crush:

- `NotebookEdit` (Jupyter), `REPL`, `PowerShell`
- `AskUserQuestion` (mid-execution structured prompts)
- `ScheduleCron` / `RemoteTrigger`
- `ToolSearch` (dynamic/deferred MCP tool discovery)
- `EnterPlanMode` / `ExitPlanMode` (Crush has plan mode as a permission-layer
  toggle rather than agent-callable tools)
- `Enter/ExitWorktree`

At parity or ahead:

- File tools: read/write/edit/multiedit/glob/grep
- Bash + background jobs (`job_output`/`job_kill`)
- MCP client (list/read resources, tools, auth)
- LSP integration exposed as dedicated tools (diagnostics, references, restart)
- Web fetch; web search (⚠️ partial — DuckDuckGo scraping, not on the main
  coder agent)
- Skills, Hooks (PreToolUse), permissions, sourcegraph search, multi-provider

## Smaller services Crush lacks

- Prompt / follow-up suggestions
- Contextual tips, away/agent summaries
- `autoDream` background ideation
- Policy/enterprise managed settings sync, x402 payments

## Suggested priorities

Highest-leverage features that fit Crush's existing architecture:

1. **Plan Mode** — small, high-impact, no new infra. *(in progress)*
2. **Explore sub-agent preset** (the "deep research" agent) — sub-agent
   plumbing already exists.
3. **Managed Task tool** layered on the existing background-job infra.
4. **Worktree create/isolate** — worktree-root resolution already exists.
