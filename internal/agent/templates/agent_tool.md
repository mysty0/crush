Launch a new sub-agent to handle a task on your behalf. Use this to offload focused work — searching the codebase, gathering context, or carrying out a self-contained change — so your main context stays clean.

The sub-agent runs in its own session and reports back only its final text output. If you need to review the sub-agent's full step-by-step activity (which files it read, what it searched for, its reasoning), run `crush session last --json` (or `crush session show <session-id> --json`) in a Bash call: the current conversation's tool_call for this agent invocation will include a `sub_agent` field embedding that sub-agent's complete transcript.

The `mode` parameter controls what the sub-agent can do:
- `read` (default): a read-only agent with Glob, Grep, ls, Read, sourcegraph, and web fetch. Use it to search for a keyword or file, gather context, or answer a question when you are not confident you will find the right match on the first try.
- `write`: an agent that can additionally edit files and run shell commands to carry out a self-contained task end to end. Use it only when the task requires making changes. It cannot launch further sub-agents.

If a sub-agent call fails or is interrupted (e.g. a canceled tool call), the error message includes that session's ID. Retry the same task with `resume_session_id` set to that ID to continue from where it left off — with the full prior message history and progress intact — instead of starting over from scratch.
