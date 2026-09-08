package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFormatElapsed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"seconds", 12 * time.Second, "12s"},
		{"just under a minute", 59 * time.Second, "59s"},
		{"minute boundary pads seconds", 65 * time.Second, "1m05s"},
		{"minutes", 150 * time.Second, "2m30s"},
		{"just under an hour", 59*time.Minute + 59*time.Second, "59m59s"},
		{"hour boundary pads minutes", time.Hour + 5*time.Minute, "1h05m"},
		{"many hours", 3*time.Hour + 25*time.Minute, "3h25m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, FormatElapsed(tc.in))
		})
	}
}

// TestElapsedIsEmptyWhenIdle verifies the turn timer reports nothing
// between turns, so callers can treat "" as "no turn running" rather
// than rendering a zero duration.
//
// Not parallel: the turn timer is process wide.
func TestElapsedIsEmptyWhenIdle(t *testing.T) {
	StopTurn()
	require.Empty(t, Elapsed())
}

// TestElapsedUsesSharedFormat guards that the turn total is spelled the
// same way as the per-step stopwatches it renders beside. A turn running
// for over a minute must read "1m05s", not "1m 5s".
//
// Not parallel: the turn timer is process wide.
func TestElapsedUsesSharedFormat(t *testing.T) {
	StartTurn()
	t.Cleanup(StopTurn)

	turnTimer.mu.Lock()
	turnTimer.startTime = time.Now().Add(-65 * time.Second)
	turnTimer.mu.Unlock()

	require.Equal(t, "1m05s", Elapsed())
}
