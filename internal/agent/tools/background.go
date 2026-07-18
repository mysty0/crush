package tools

import "sync"

// BackgroundKind identifies what kind of blocking operation was sent to
// the background by a Ctrl+B request, so the UI can report a breakdown
// (e.g. "1 bash command, 2 sub-agents") rather than a bare count.
type BackgroundKind string

const (
	// BackgroundKindBash is a foreground bash command's synchronous wait.
	BackgroundKindBash BackgroundKind = "bash command"
	// BackgroundKindSubAgent is a synchronous sub-agent turn dispatched by
	// the "agent" or "agentic_fetch" tool (see coordinator.runSubAgent).
	BackgroundKindSubAgent BackgroundKind = "sub-agent"
)

// BackgroundTrigger is handed to a blocking operation by RegisterBackground.
// Its channel closes when BackgroundNow fires for the session it was
// registered under, so the operation can select on C() alongside its other
// wait conditions (completion, timeout, cancellation) and detach itself.
type BackgroundTrigger struct {
	ch chan struct{}
}

// C returns the channel that closes when this operation should detach and
// continue running in the background.
func (t *BackgroundTrigger) C() <-chan struct{} {
	return t.ch
}

type backgroundKey struct {
	sessionID  string
	toolCallID string
}

type backgroundEntry struct {
	trigger *BackgroundTrigger
	kind    BackgroundKind
}

// backgroundRegistry is a package-level singleton, in the same style as
// bashProgressBroker, keyed by (sessionID, toolCallID) so multiple
// concurrent blocking operations in one session (e.g. parallel agent()
// calls in a single step) can each be registered and fired independently,
// while a single BackgroundNow(sessionID) call fires all of them at once.
var (
	backgroundMu       sync.Mutex
	backgroundRegistry = map[backgroundKey]backgroundEntry{}
)

// RegisterBackground registers a blocking tool call as backgroundable and
// returns a trigger that closes its channel when BackgroundNow fires for
// this session, plus an unregister func the caller must invoke (typically
// via defer) once the operation finishes on its own. unregister is safe to
// call even after BackgroundNow has already fired and removed the entry
// itself.
func RegisterBackground(sessionID, toolCallID string, kind BackgroundKind) (trigger *BackgroundTrigger, unregister func()) {
	key := backgroundKey{sessionID: sessionID, toolCallID: toolCallID}
	t := &BackgroundTrigger{ch: make(chan struct{})}

	backgroundMu.Lock()
	backgroundRegistry[key] = backgroundEntry{trigger: t, kind: kind}
	backgroundMu.Unlock()

	return t, func() {
		backgroundMu.Lock()
		delete(backgroundRegistry, key)
		backgroundMu.Unlock()
	}
}

// BackgroundNow signals every operation currently registered for
// sessionID to detach and continue running in the background. It returns
// how many were fired, broken down by BackgroundKind, so the caller can
// report a precise summary (e.g. "backgrounded 1 bash command, 2
// sub-agents"). A nil/empty result means nothing was blocking.
func BackgroundNow(sessionID string) map[BackgroundKind]int {
	backgroundMu.Lock()
	defer backgroundMu.Unlock()

	var fired map[BackgroundKind]int
	for key, entry := range backgroundRegistry {
		if key.sessionID != sessionID {
			continue
		}
		close(entry.trigger.ch)
		if fired == nil {
			fired = map[BackgroundKind]int{}
		}
		fired[entry.kind]++
		delete(backgroundRegistry, key)
	}
	return fired
}
