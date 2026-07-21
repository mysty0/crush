Fetch a URL or search the web using an AI sub-agent that can extract, summarize, and answer questions. Slower and costlier than fetch; use fetch for raw content or API responses.

Set `background: true` to start the fetch/analysis and return immediately instead of waiting for it to finish. Its result is delivered later as a follow-up message in this conversation; check on progress any time with `AgentList` or `AgentProgress(session_id)`.
