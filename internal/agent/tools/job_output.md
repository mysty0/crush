Get stdout/stderr from a background shell by ID.

Set wait=true to block until the shell completes. The wait is time-bounded
(default 60s, max 600s via the timeout parameter): if the job is still running
when the timeout elapses, the call returns the current output with status
"running" instead of blocking forever. Use job_kill to stop a job that will
never finish on its own (a server, a watcher, an interactive prompt).
