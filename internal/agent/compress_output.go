package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
)

// compressThreshold is the target keep-rate passed to headroomd's compress
// method: roughly how much of the original text's information content the
// daemon should try to retain. 0.5 favors context savings over fidelity,
// on the assumption that a compressed prior-step tool result is background
// context the model can recover in full via retrieve_full_output if it
// turns out to matter.
const compressThreshold = 0.5

// compressedOutputCache memoizes the replacement text produced for a given
// (session, content) pair, so re-compressing the same tool result on a
// later turn skips the daemon round-trip entirely.
//
// It is only an optimization: turn-to-turn byte stability comes from the
// content-derived retrieval id in buildCompressToolOutput, not from this
// cache, so a miss (first turn, rebuilt coordinator) still renders
// identical bytes -- headroomd's compression is a deterministic threshold
// over per-token keep scores, so the same text always compresses the same
// way. The zero value is ready to use.
type compressedOutputCache struct {
	mu    sync.Mutex
	byKey map[string]string
}

func (c *compressedOutputCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	replacement, ok := c.byKey[key]
	return replacement, ok
}

func (c *compressedOutputCache) put(key, replacement string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = make(map[string]string)
	}
	c.byKey[key] = replacement
}

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
//
// Compression is deliberately ephemeral: it rewrites only the copy of the
// message slice built for the current provider call, never the persisted
// message, so every turn re-compresses the same stored tool output from
// scratch. That makes byte-for-byte stability a hard requirement rather
// than a nicety -- providers with prompt caching key on an exact request
// prefix, so rendering a prior tool result even one byte differently than
// last turn invalidates the cache from that message onwards, for the rest
// of the session. Hence the retrieval id is derived from the content
// (identical content -> identical id) instead of being freshly generated
// per call. Content-addressing also makes the retrieveStore write
// idempotent, so a session accumulates one entry per distinct tool output
// rather than a fresh copy on every turn.
func (c *coordinator) buildCompressToolOutput(isSubAgent bool) func(ctx context.Context, sessionID, content string) (string, bool) {
	if isSubAgent || c.compressdMgr == nil {
		return nil
	}
	return func(ctx context.Context, sessionID, content string) (string, bool) {
		opts := c.cfg.Config().Options
		if opts.CompressToolOutputs != nil && !*opts.CompressToolOutputs {
			return "", false
		}

		sum := sha256.Sum256([]byte(content))
		id := hex.EncodeToString(sum[:16])
		// Keyed by session as well as content because the retrieveStore
		// entry backing the id is session-scoped: a hit here skips the
		// Put below, which is only sound if this session already has it.
		cacheKey := sessionID + ":" + id
		if replacement, ok := c.compressCache.get(cacheKey); ok {
			return replacement, true
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

		c.retrieveStore.Put(sessionID, id, content)
		replacement := fmt.Sprintf(
			"[Tool output compressed to save context (kept ~%.0f%%). "+
				"Use retrieve_full_output with id=%q to see the original if needed.]\n\n%s",
			keepRate*100, id, compressed,
		)
		c.compressCache.put(cacheKey, replacement)
		return replacement, true
	}
}
