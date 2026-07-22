package agent

import (
	"context"
	"log/slog"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/message"
)

// backgroundUsageParent is the sentinel parent id given to the hidden
// per-project sessions that record auxiliary (non-conversational) model
// spend. It is non-empty solely so parent_session_id is non-NULL and the
// session is therefore excluded from the top-level session list
// (ListSessions filters on parent_session_id IS NULL), while its usage
// stays queryable in the database.
const backgroundUsageParent = "background-usage"

// Sources for background (non-conversational) model calls, used both as the
// telemetry "source" label and to key each call's hidden per-project
// session so different kinds of background spend stay separable.
const (
	bgSourceMemoryJudge    = "memory_relevance_judge"
	bgSourceWebSearch      = "web_search"
	bgSourceWebFetch       = "web_fetch"
	bgSourceWorkflowObject = "workflow_object"
)

// backgroundUsageSessionID is the stable per-project, per-source session id
// under which a given kind of background model call accumulates its usage.
func backgroundUsageSessionID(source, projectScope string) string {
	return source + "-" + projectScope
}

// recordBackgroundUsage persists the token usage of an auxiliary model call
// -- native web search, web-fetch summarize, the workflow structured-output
// helpers, or the memory-relevance judge -- so this background subscription
// spend is counted rather than invisible. Each source accumulates into its
// own hidden per-project session: an assistant message carrying a
// TokenUsage part is appended, the session's usage counters accumulate, and
// the same "tokens used" telemetry a normal turn emits is sent. It is
// best-effort -- any failure is logged and swallowed, never propagated,
// since these calls run outside the main conversation.
func (c *coordinator) recordBackgroundUsage(ctx context.Context, model Model, projectScope, source, title, summary string, usage fantasy.Usage) {
	if usageIsZero(usage) {
		return
	}

	mc := model.CatwalkCfg
	cost := mc.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		mc.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		mc.CostPer1MIn/1e6*float64(usage.InputTokens) +
		mc.CostPer1MOut/1e6*float64(usage.OutputTokens)
	if model.FlatRate {
		cost = 0
	}

	sessionID := backgroundUsageSessionID(source, projectScope)
	if _, err := c.sessions.Get(ctx, sessionID); err != nil {
		if _, cerr := c.sessions.CreateTaskSession(ctx, sessionID, backgroundUsageParent, title); cerr != nil {
			// A concurrent call may have created it first; only give up if
			// it still does not exist.
			if _, gerr := c.sessions.Get(ctx, sessionID); gerr != nil {
				slog.Debug("Background usage: could not open session", "source", source, "err", cerr)
				return
			}
		}
	}

	parts := make([]message.ContentPart, 0, 3)
	if summary != "" {
		parts = append(parts, message.TextContent{Text: summary})
	}
	parts = append(parts,
		message.TokenUsage{
			InputTokens:         usage.InputTokens,
			OutputTokens:        usage.OutputTokens,
			CacheReadTokens:     usage.CacheReadTokens,
			CacheCreationTokens: usage.CacheCreationTokens,
			Cost:                cost,
		},
		message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
	)
	if _, err := c.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    parts,
		Model:    model.ModelCfg.Model,
		Provider: model.ModelCfg.Provider,
	}); err != nil {
		slog.Debug("Background usage: could not record message", "source", source, "err", err)
	}

	promptTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	if err := c.sessions.UpdateTitleAndUsage(ctx, sessionID, title, promptTokens, usage.OutputTokens, usage.CacheCreationTokens, usage.CacheReadTokens, cost); err != nil {
		slog.Debug("Background usage: could not update session usage", "source", source, "err", err)
	}

	event.TokensUsed(
		"session id", sessionID,
		"provider", model.ModelCfg.Provider,
		"model", model.ModelCfg.Model,
		"input tokens", usage.InputTokens,
		"output tokens", usage.OutputTokens,
		"cache read tokens", usage.CacheReadTokens,
		"cache creation tokens", usage.CacheCreationTokens,
		"total tokens", usage.InputTokens+usage.OutputTokens+usage.CacheReadTokens+usage.CacheCreationTokens,
		"cost", cost,
		"source", source,
	)
}
