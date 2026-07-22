package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// compressThreshold is the target keep-rate passed to headroomd's compress
// method: roughly how much of the original text's information content the
// daemon should try to retain. 0.5 favors context savings over fidelity,
// on the assumption that a compressed prior-step tool result is background
// context the model can recover in full via retrieve_full_output if it
// turns out to matter.
const compressThreshold = 0.5

// buildCompressToolOutput returns the per-message tool-output compression
// function passed to a SessionAgent, or nil when this is a sub-agent (a
// focused, short-lived task where compression rarely pays for itself) or
// no compressd.Manager was wired up (e.g. tests, or a build that doesn't
// need this feature).
//
// The returned function re-reads the CompressToolOutputs config option on
// every call (rather than once at construction) so a config reload takes
// effect without restarting the agent, matching how other per-turn config
// reads in this package behave (see e.g. DisableAutoSummarize consumers).
func (c *coordinator) buildCompressToolOutput(isSubAgent bool) func(ctx context.Context, sessionID, content string) (string, bool) {
	if isSubAgent || c.compressdMgr == nil {
		return nil
	}
	return func(ctx context.Context, sessionID, content string) (string, bool) {
		opts := c.cfg.Config().Options
		if opts.CompressToolOutputs != nil && !*opts.CompressToolOutputs {
			return "", false
		}

		client, ok := c.compressdMgr.Client(ctx)
		if !ok {
			return "", false
		}

		compressed, keepRate, _, err := client.Compress(ctx, content, compressThreshold)
		if err != nil {
			slog.Debug("Tool-output compression failed; leaving content untouched", "error", err)
			return "", false
		}

		id := uuid.NewString()
		c.retrieveStore.Put(sessionID, id, content)
		replacement := fmt.Sprintf(
			"[Tool output compressed to save context (kept ~%.0f%%). "+
				"Use retrieve_full_output with id=%q to see the original if needed.]\n\n%s",
			keepRate*100, id, compressed,
		)
		return replacement, true
	}
}
