package common

import (
	"fmt"
	"sync"
	"time"
)

// turnTimer tracks the elapsed time for the current agent turn.
var turnTimer struct {
	mu        sync.Mutex
	startTime time.Time
	active    bool
}

// StartTurn begins tracking elapsed time for a new turn.
func StartTurn() {
	turnTimer.mu.Lock()
	defer turnTimer.mu.Unlock()
	turnTimer.startTime = time.Now()
	turnTimer.active = true
}

// StopTurn stops tracking the current turn.
func StopTurn() {
	turnTimer.mu.Lock()
	defer turnTimer.mu.Unlock()
	turnTimer.active = false
}

// Elapsed returns the formatted elapsed time for the current turn.
// Returns empty string if no turn is active.
func Elapsed() string {
	turnTimer.mu.Lock()
	defer turnTimer.mu.Unlock()
	if !turnTimer.active {
		return ""
	}
	return FormatElapsed(time.Since(turnTimer.startTime))
}

// FormatElapsed renders a whole-second duration compactly: "12s" under a
// minute, "1m05s" under an hour, "1h05m" beyond that.
//
// This is the single elapsed-time format across the UI. The turn total
// and the per-step stopwatches sit side by side on the same loader line
// (see chat.AssistantMessageItem), so two spellings of the same duration
// there would read as two different measurements.
func FormatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
}
