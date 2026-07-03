Run a multi-phase workflow (e.g. deep-research) that fans out many sub-agent calls to search, fetch, verify, and synthesize a result. Slower and costlier than a single agent call — use it when the task explicitly calls for the workflow's structured multi-step process, not for simple lookups.

Available workflows:
{{range .Workflows}}
- **{{.Name}}**: {{.Description}} {{.WhenToUse}}
{{end}}
