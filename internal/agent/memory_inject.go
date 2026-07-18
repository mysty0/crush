package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/memory"
)

// memoryInjectLimit caps how many memories are injected per turn.
const memoryInjectLimit = 6

// memoryJudgeTimeout bounds the background relevance-judge call so a slow
// or hung small-model provider can never leak a goroutine indefinitely.
const memoryJudgeTimeout = 20 * time.Second

// memoryJudgeSystemPrompt instructs the small model to classify recalled
// memories as relevant or not to the task they were injected for. Kept as
// a plain constant rather than a template file: this is an internal,
// structured classification call, not a conversational persona prompt.
const memoryJudgeSystemPrompt = "You judge whether previously recorded facts are actually useful for a specific task. " +
	"Given a task and a numbered list of facts, reply with ONLY the numbers of the facts that are directly relevant " +
	"and worth acting on for this specific task, comma-separated (e.g. \"1,3\"). If none apply, reply with \"none\". " +
	"Do not explain your reasoning."

// buildMemoryRecall returns the per-turn memory-injection function passed to
// a SessionAgent, plus a reset function to call after a session summarizes,
// or (nil, nil) when memory is off or this is a sub-agent (sub-agents run
// focused, isolated tasks and do not carry the project's long-term memory).
//
// The recall function is queried fresh on every user turn, and a broad,
// generic memory (e.g. a standing "always/never do X" instruction) can keep
// clearing the relevance bar turn after turn even once the model has judged
// it irrelevant to the task at hand — repeatedly spending tokens and
// attention on a fact already visible earlier in the same conversation. To
// avoid that, each recalled memory is injected at most once per session;
// resetMemoryShown clears that per-session record after a summarize, since
// summarization compacts earlier turns away and the memory may be worth
// surfacing again.
//
// Beyond that in-session dedup, injected memories are judged for relevance
// in the background: recall never waits on this, so injection is never
// slowed down. But the judge is not called on every injection either --
// each memory tracks (in the store) how many times it has actually been
// injected since its last judgment, and is only judged again once that
// count reaches its current backoff interval. A confirmed-relevant memory
// doubles its interval (judged less and less often); a rejected one resets
// to the minimum (re-tested soon). This is the mechanism that keeps the
// small model from being called on every single turn a settled memory
// happens to surface -- see memory.Store.BumpJudgeCounter and
// ReinforceRelevance for the actual backoff bookkeeping. An in-flight guard
// additionally prevents two concurrent sessions from both judging the same
// due memory at once.
func (c *coordinator) buildMemoryRecall(isSubAgent bool, small Model) (recall func(ctx context.Context, sessionID, query string) string, resetShown func(sessionID string)) {
	if isSubAgent || c.memory == nil || !c.cfg.Config().Options.MemoryEnabled() {
		return nil, nil
	}
	projectScope := memory.ProjectScope(c.cfg.WorkingDir())
	shownBySession := csync.NewMap[string, *csync.Map[string, struct{}]]()
	judging := csync.NewMap[string, struct{}]()
	smallProviderCfg, _ := c.cfg.Config().Providers.Get(small.ModelCfg.Provider)

	recall = func(ctx context.Context, sessionID, query string) string {
		hits, err := c.memory.Recall(ctx, []string{projectScope, memory.ScopeGlobal}, query, memoryInjectLimit)
		if err != nil || len(hits) == 0 {
			return ""
		}
		shown := shownBySession.GetOrSet(sessionID, func() *csync.Map[string, struct{}] {
			return csync.NewMap[string, struct{}]()
		})
		fresh := hits[:0]
		for _, h := range hits {
			if _, ok := shown.Get(h.ID); ok {
				continue
			}
			shown.Set(h.ID, struct{}{})
			fresh = append(fresh, h)
		}
		if len(fresh) == 0 {
			return ""
		}

		if small.Model != nil {
			due := dueForJudge(ctx, c.memory, judging, fresh)
			if len(due) > 0 {
				go c.judgeMemoryRelevance(small, smallProviderCfg.SystemPromptPrefix, query, due, judging)
			}
		}

		return renderMemoryBlock(fresh)
	}
	resetShown = func(sessionID string) {
		shownBySession.Del(sessionID)
	}
	return recall, resetShown
}

// dueForJudge bumps each freshly-injected memory's recall-since-last-judge
// counter and returns the subset that has now reached its backoff interval
// and is not already being judged by a concurrent call (guarded by
// judging). Claimed hits are marked in judging immediately so a second,
// near-simultaneous recall (e.g. from another session in the same project)
// does not also queue the same memory.
func dueForJudge(ctx context.Context, store *memory.Store, judging *csync.Map[string, struct{}], fresh []memory.Hit) []memory.Hit {
	var due []memory.Hit
	for _, h := range fresh {
		isDue, err := store.BumpJudgeCounter(ctx, h.ID)
		if err != nil || !isDue {
			continue
		}
		if _, alreadyClaimed := judging.Get(h.ID); alreadyClaimed {
			continue
		}
		judging.Set(h.ID, struct{}{})
		due = append(due, h)
	}
	return due
}

// judgeMemoryRelevance asks the small model which of the due memories
// actually mattered for query, then reinforces or penalizes each one's
// importance and judge backoff interval accordingly via
// memory.Store.ReinforceRelevance. It runs on a context decoupled from the
// turn that triggered it (so it is not canceled when that turn finishes),
// bounded by memoryJudgeTimeout, and fails silently -- no change -- on any
// error, timeout, or unparseable response: a wrong or missing verdict must
// never be worse than not judging at all. It also recovers from any panic:
// this runs detached in the background, so an unexpected failure here (a
// bad provider config, a malformed response) must never crash the process.
// Always releases every hit's judging claim on return, however it exits.
func (c *coordinator) judgeMemoryRelevance(small Model, systemPromptPrefix, query string, hits []memory.Hit, judging *csync.Map[string, struct{}]) {
	defer func() {
		for _, h := range hits {
			judging.Del(h.ID)
		}
		if r := recover(); r != nil {
			slog.Debug("Memory relevance judge panicked; leaving importance unchanged", "recovered", r)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), memoryJudgeTimeout)
	defer cancel()

	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(h.Content))
	}

	systemPrompt := memoryJudgeSystemPrompt
	if systemPromptPrefix != "" {
		systemPrompt = systemPromptPrefix + "\n\n" + systemPrompt
	}

	judge := fantasy.NewAgent(
		small.Model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithMaxOutputTokens(30),
		fantasy.WithUserAgent(userAgent),
		fantasy.WithMaxRetries(providerMaxRetries),
	)

	resp, err := judge.Stream(ctx, fantasy.AgentStreamCall{
		Prompt: fmt.Sprintf("Task: %s\n\nFacts:\n%s", query, b.String()),
	})
	if err != nil {
		slog.Debug("Memory relevance judge failed; leaving importance unchanged", "err", err)
		return
	}

	relevant := parseRelevanceVerdict(resp.Response.Content.Text(), len(hits))
	if relevant == nil {
		slog.Debug("Memory relevance judge returned an unparseable response; leaving importance unchanged",
			"reply", resp.Response.Content.Text())
		return
	}

	confirmed, rejected := 0, 0
	for i, h := range hits {
		if relevant[i] {
			confirmed++
		} else {
			rejected++
		}
		if err := c.memory.ReinforceRelevance(ctx, h.ID, relevant[i]); err != nil {
			slog.Debug("Failed to reinforce memory relevance", "err", err)
		}
	}
	slog.Debug("Memory relevance judge completed", "judged", len(hits), "confirmed", confirmed, "rejected", rejected)
}

// parseRelevanceVerdict parses the judge's reply (a comma-separated list of
// 1-based fact numbers, or "none") into a per-hit relevant/irrelevant slice
// of length n. Returns nil if the reply can't be parsed at all, so the
// caller leaves every memory's importance untouched rather than risk
// misreading a malformed or off-format response as "everything irrelevant."
func parseRelevanceVerdict(reply string, n int) []bool {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return nil
	}
	relevant := make([]bool, n)
	if strings.EqualFold(reply, "none") {
		return relevant
	}
	found := false
	for _, tok := range strings.Split(reply, ",") {
		tok = strings.TrimSpace(tok)
		idx, err := strconv.Atoi(tok)
		if err != nil || idx < 1 || idx > n {
			continue
		}
		relevant[idx-1] = true
		found = true
	}
	if !found {
		return nil
	}
	return relevant
}

// renderMemoryBlock formats recalled memories as an injected context block. It is
// framed as advisory so the model never trusts a stale memory over live code,
// and it explicitly tells the model to apply or discard each memory silently
// rather than narrate about it. Retrieval is lexical (keyword/BM25-based), so
// a memory sharing real vocabulary with an unrelated problem (e.g. two
// different graphics/monitor issues) can still surface even though its
// specific cause doesn't apply here; without this instruction the model tends
// to explicitly re-litigate each injected memory's relevance in its visible
// reasoning every time it comes up, which is noisy and off-putting when the
// memory turns out not to apply.
func renderMemoryBlock(hits []memory.Hit) string {
	var b strings.Builder
	b.WriteString("<memory>\n")
	b.WriteString("Relevant facts you recorded in earlier sessions about this project or the user. ")
	b.WriteString("They may be out of date or simply not apply to the current problem — the current code, files, ")
	b.WriteString("and the user always win. Weigh each one silently: use it if it applies, otherwise disregard it ")
	b.WriteString("without commenting on it or explaining why it doesn't apply. Only mention a memory to the user ")
	b.WriteString("if it directly changes what you do next. Use the Forget tool if one is wrong.\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(h.Content))
	}
	b.WriteString("</memory>")
	return b.String()
}
