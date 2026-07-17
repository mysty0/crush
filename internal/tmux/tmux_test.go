package tmux

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// writeFakeTmux plants a fake "tmux" script on PATH (via t.Setenv) that
// fails for its first failUntil invocations (tracked via a counter file,
// since each invocation is a fresh process) and then prints "sess:0.0" and
// exits 0. It returns the path to the counter file so callers can assert
// how many times the fake was actually invoked.
func writeFakeTmux(t *testing.T, failUntil int) (counterFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("relies on a Unix shell script standing in for tmux")
	}
	dir := t.TempDir()
	counterFile = filepath.Join(dir, "count")
	script := "#!/bin/sh\n" +
		"n=0\n" +
		"[ -f \"" + counterFile + "\" ] && n=$(cat \"" + counterFile + "\")\n" +
		"n=$((n+1))\n" +
		"echo \"$n\" > \"" + counterFile + "\"\n" +
		"if [ \"$n\" -le " + strconv.Itoa(failUntil) + " ]; then\n" +
		"  exit 1\n" +
		"fi\n" +
		"echo \"sess:0.0\"\n"
	fake := filepath.Join(dir, "tmux")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	// Prepend (not replace): the fake script itself shells out to "cat", a
	// real external binary, to read the counter file. Replacing PATH wholesale
	// leaves "cat" unresolvable, so the script silently starts from n=0 on
	// every invocation and the counter never advances past 1 -- a red herring
	// that looks exactly like PaneKey not retrying at all.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counterFile
}

func readCount(t *testing.T, counterFile string) int {
	t.Helper()
	data, err := os.ReadFile(counterFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read counter file: %v", err)
	}
	n, err := strconv.Atoi(string(data[:len(data)-1])) // trim trailing newline
	if err != nil {
		t.Fatalf("parse counter file %q: %v", data, err)
	}
	return n
}

// withFastRetries shrinks the retry knobs for the duration of the test so
// exercising the retry loop doesn't slow the suite down, and restores the
// production defaults afterward.
func withFastRetries(t *testing.T, maxRetries int) {
	t.Helper()
	origMax, origDelay := paneKeyMaxRetries, paneKeyRetryDelay
	paneKeyMaxRetries = maxRetries
	paneKeyRetryDelay = time.Millisecond
	t.Cleanup(func() {
		paneKeyMaxRetries, paneKeyRetryDelay = origMax, origDelay
	})
}

// TestPaneKeySucceedsFirstTryFast guards the common (non-restore) case: a
// healthy tmux server answers on the first attempt with no retries.
func TestPaneKeySucceedsFirstTryFast(t *testing.T) {
	withFastRetries(t, 4)
	counter := writeFakeTmux(t, 0)

	key, err := PaneKey(context.Background())
	if err != nil {
		t.Fatalf("PaneKey() error = %v", err)
	}
	if key != "sess:0.0" {
		t.Fatalf("PaneKey() = %q, want %q", key, "sess:0.0")
	}
	if got := readCount(t, counter); got != 1 {
		t.Fatalf("tmux invoked %d times, want exactly 1 (no retry needed)", got)
	}
}

// TestPaneKeyRetriesTransientFailure reproduces the tmux-resurrect restore
// scenario this retry loop exists for: the first couple of calls to
// "tmux display-message" fail (simulating a busy/restarting server) and a
// later one succeeds. PaneKey must absorb the transient failures and still
// return the correct value.
func TestPaneKeyRetriesTransientFailure(t *testing.T) {
	withFastRetries(t, 4)
	counter := writeFakeTmux(t, 2) // fails twice, succeeds on the 3rd call

	key, err := PaneKey(context.Background())
	if err != nil {
		t.Fatalf("PaneKey() error = %v, want it to recover after transient failures", err)
	}
	if key != "sess:0.0" {
		t.Fatalf("PaneKey() = %q, want %q", key, "sess:0.0")
	}
	if got := readCount(t, counter); got != 3 {
		t.Fatalf("tmux invoked %d times, want 3 (2 failures + 1 success)", got)
	}
}

// TestPaneKeyGivesUpAfterMaxRetries verifies PaneKey does not retry forever:
// once every attempt has failed, it returns an error after exactly
// maxRetries+1 attempts (the initial try plus each retry).
func TestPaneKeyGivesUpAfterMaxRetries(t *testing.T) {
	withFastRetries(t, 3)
	counter := writeFakeTmux(t, 1000) // always fails

	_, err := PaneKey(context.Background())
	if err == nil {
		t.Fatal("PaneKey() error = nil, want an error once retries are exhausted")
	}
	if got, want := readCount(t, counter), 4; got != want {
		t.Fatalf("tmux invoked %d times, want %d (1 initial + 3 retries)", got, want)
	}
}

// TestPaneKeyRespectsContextCancellation verifies a canceled context stops
// the retry loop during the backoff wait instead of exhausting all retries.
func TestPaneKeyRespectsContextCancellation(t *testing.T) {
	origMax, origDelay := paneKeyMaxRetries, paneKeyRetryDelay
	paneKeyMaxRetries = 5
	paneKeyRetryDelay = time.Hour // long enough to prove cancellation, not a timeout, unblocks the wait
	t.Cleanup(func() { paneKeyMaxRetries, paneKeyRetryDelay = origMax, origDelay })
	writeFakeTmux(t, 1000) // always fails

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := PaneKey(ctx)
	if err == nil {
		t.Fatal("PaneKey() error = nil, want context.Canceled")
	}
}
