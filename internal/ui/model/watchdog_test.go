package model

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStallWatchdog_DumpsOnStall verifies that when a monitored section
// stays "in flight" longer than the threshold, the watchdog writes a
// goroutine dump into its directory.
func TestStallWatchdog_DumpsOnStall(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &stallWatchdog{
		threshold:    50 * time.Millisecond,
		interval:     10 * time.Millisecond,
		dumpCooldown: time.Second,
		dumpDir:      dir,
	}
	w.start()

	// Enter a section and never leave it, simulating a stuck Update/View.
	w.enter("Update test.stallMsg")

	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(dir)
		return err == nil && len(entries) > 0
	}, 2*time.Second, 20*time.Millisecond, "expected a stall dump to be written")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	require.NoError(t, err)
	require.Contains(t, string(data), "Crush UI stall detected")
	require.Contains(t, string(data), "Update test.stallMsg")
}

// TestStallWatchdog_NoDumpWhenIdle verifies that a watchdog whose monitored
// sections complete quickly never writes a dump.
func TestStallWatchdog_NoDumpWhenIdle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &stallWatchdog{
		threshold:    50 * time.Millisecond,
		interval:     10 * time.Millisecond,
		dumpCooldown: time.Second,
		dumpDir:      dir,
	}
	w.start()

	// Enter and immediately leave, repeatedly — never stalls.
	for range 5 {
		w.enter("Update ok")()
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "no dump should be written when sections complete quickly")
}

// TestStallWatchdog_NilSafe verifies the no-op behavior on a nil watchdog.
func TestStallWatchdog_NilSafe(t *testing.T) {
	t.Parallel()

	var w *stallWatchdog
	w.start()                 // must not panic
	done := w.enter("Update") // must return a usable func
	done()                    // must not panic
}

// TestNewStallWatchdog_DisabledByEnv verifies the kill switch.
func TestNewStallWatchdog_DisabledByEnv(t *testing.T) {
	t.Setenv("CRUSH_STALL_WATCHDOG", "off")
	require.Nil(t, newStallWatchdog(t.TempDir()))
}
