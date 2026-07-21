package agent

import (
	"context"
	"log/slog"

	"charm.land/fantasy"
)

// cancelEnforcingTool wraps a fantasy.AgentTool so that a run cancellation
// always returns control to the caller, even when the wrapped tool ignores
// context cancellation or is wedged in a blocking syscall it cannot be
// interrupted from (e.g. reading a FIFO, a socket, or a regular file on a
// hung network mount).
//
// Go cannot preempt or kill a goroutine stuck in a blocking syscall from the
// outside; context cancellation is cooperative and only works if the callee
// checks it. The fantasy agent step waits for every dispatched tool goroutine
// to return before it can finish, so a single non-returning tool wedges the
// whole turn: the session never leaves the busy state and the user's cancel
// (Esc/Ctrl+C) has nothing to act on. This decorator breaks that dependency
// by racing the tool against the run context: on cancellation it returns
// promptly with the context error and abandons the still-running goroutine,
// letting the turn unwind. The existing cancel path (errors.Is(err,
// context.Canceled) in sessionAgent.Run) then persists the turn as canceled.
//
// An abandoned goroutine (and any file descriptor it holds) leaks until the
// blocking call eventually returns or the process exits. That is a bounded,
// rare cost — logged for visibility — and far preferable to a permanent hang.
type cancelEnforcingTool struct {
	inner fantasy.AgentTool
}

// toolOutcome carries a wrapped tool's result off the goroutine that ran it.
type toolOutcome struct {
	resp fantasy.ToolResponse
	err  error
}

func (t *cancelEnforcingTool) Info() fantasy.ToolInfo {
	return t.inner.Info()
}

func (t *cancelEnforcingTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *cancelEnforcingTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *cancelEnforcingTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	// Buffered so the abandoned goroutine's send never blocks (and never
	// panics: the channel is never closed) if we have already returned on
	// cancellation.
	ch := make(chan toolOutcome, 1)
	go func() {
		resp, err := t.inner.Run(ctx, call)
		ch <- toolOutcome{resp: resp, err: err}
	}()

	select {
	case out := <-ch:
		return out.resp, out.err
	case <-ctx.Done():
		slog.Warn(
			"Tool did not return on cancellation; abandoning it and returning control",
			"tool", t.inner.Info().Name,
			"call_id", call.ID,
			"error", ctx.Err(),
		)
		// Return the context error so the run's existing cancel handling
		// (errors.Is(err, context.Canceled)) fires and the turn is
		// persisted as canceled rather than as a tool failure shown to the
		// model.
		return fantasy.ToolResponse{}, ctx.Err()
	}
}

// wrapToolsWithCancellation wraps every tool so a run cancellation always
// returns control to the caller, regardless of whether the underlying tool
// honors context cancellation. This must be the outermost wrapper so it races
// the entire decorator chain (hooks, plan mode, error tagging, and the tool
// itself) against the run context — a wedge anywhere in that chain is then
// still escapable.
func wrapToolsWithCancellation(ts []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(ts))
	for i, tool := range ts {
		out[i] = &cancelEnforcingTool{inner: tool}
	}
	return out
}
