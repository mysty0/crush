package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/memory"
)

// memoryInjectLimit caps how many memories are injected per turn.
const memoryInjectLimit = 6

// buildMemoryRecall returns the per-turn memory-injection function passed to a
// SessionAgent, or nil when memory is off or this is a sub-agent (sub-agents
// run focused, isolated tasks and do not carry the project's long-term memory).
func (c *coordinator) buildMemoryRecall(isSubAgent bool) func(context.Context, string) string {
	if isSubAgent || c.memory == nil || !c.cfg.Config().Options.MemoryEnabled() {
		return nil
	}
	projectScope := memory.ProjectScope(c.cfg.WorkingDir())
	return func(ctx context.Context, query string) string {
		hits, err := c.memory.Recall(ctx, []string{projectScope, memory.ScopeGlobal}, query, memoryInjectLimit)
		if err != nil || len(hits) == 0 {
			return ""
		}
		return renderMemoryBlock(hits)
	}
}

// renderMemoryBlock formats recalled memories as an injected context block. It is
// framed as advisory so the model never trusts a stale memory over live code.
func renderMemoryBlock(hits []memory.Hit) string {
	var b strings.Builder
	b.WriteString("<memory>\n")
	b.WriteString("Relevant facts you recorded in earlier sessions about this project or the user. ")
	b.WriteString("They may be out of date — the current code, files, and the user always win. ")
	b.WriteString("Use the Forget tool if one is wrong.\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(h.Content))
	}
	b.WriteString("</memory>")
	return b.String()
}
