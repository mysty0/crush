package model

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// stallWatchdog detects when the Bubble Tea event loop is stuck — i.e. a
// single Update or View call runs longer than a threshold — and dumps all
// goroutine stacks to a file so a freeze can be diagnosed after the fact.
//
// Update and View are called sequentially on the same goroutine and never
// overlap, so a single in-flight timestamp is enough to time whichever one
// is currently running. The monitor runs on its own goroutine, so it keeps
// working even while the event loop is blocked.
type stallWatchdog struct {
	threshold    time.Duration
	interval     time.Duration
	dumpCooldown time.Duration
	dumpDir      string

	// startNanos is the unix-nano time the current Update/View began, or 0
	// when the loop is idle. label describes what is in flight.
	startNanos atomic.Int64
	label      atomic.Pointer[string]

	// lastDumpNanos throttles dumps so a single long stall produces one
	// dump rather than one per monitor tick.
	lastDumpNanos atomic.Int64
}

// newStallWatchdog builds a watchdog that dumps into dumpDir. It honors two
// environment variables:
//
//   - CRUSH_STALL_WATCHDOG=off|0|false disables it entirely (returns nil).
//   - CRUSH_STALL_WATCHDOG_SECS overrides the stall threshold in seconds.
//
// A nil watchdog is safe to use: every method is a no-op on a nil receiver.
func newStallWatchdog(dumpDir string) *stallWatchdog {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CRUSH_STALL_WATCHDOG"))) {
	case "off", "0", "false", "no":
		return nil
	}

	threshold := 10 * time.Second
	if v := strings.TrimSpace(os.Getenv("CRUSH_STALL_WATCHDOG_SECS")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			threshold = time.Duration(secs) * time.Second
		}
	}

	return &stallWatchdog{
		threshold:    threshold,
		interval:     time.Second,
		dumpCooldown: 30 * time.Second,
		dumpDir:      dumpDir,
	}
}

// start launches the monitor goroutine. It is a process-lifetime daemon
// (reaped on exit); safe to call on a nil watchdog.
func (w *stallWatchdog) start() {
	if w == nil {
		return
	}
	go w.run()
}

// enter marks the start of a monitored section (an Update or View call) and
// returns a function to call — typically deferred — when it completes.
func (w *stallWatchdog) enter(label string) func() {
	if w == nil {
		return func() {}
	}
	w.label.Store(&label)
	w.startNanos.Store(time.Now().UnixNano())
	return func() { w.startNanos.Store(0) }
}

func (w *stallWatchdog) run() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for range ticker.C {
		start := w.startNanos.Load()
		if start == 0 {
			continue
		}
		elapsed := time.Since(time.Unix(0, start))
		if elapsed < w.threshold {
			continue
		}
		if last := w.lastDumpNanos.Load(); last != 0 &&
			time.Since(time.Unix(0, last)) < w.dumpCooldown {
			continue
		}
		w.lastDumpNanos.Store(time.Now().UnixNano())
		w.dump(elapsed)
	}
}

// dump writes every goroutine's stack to a timestamped file in dumpDir and
// logs where it went. It is the only place that captures the stacks, so it
// runs from the monitor goroutine while the event loop is still wedged —
// exactly the state we want to inspect.
func (w *stallWatchdog) dump(elapsed time.Duration) {
	label := "unknown"
	if p := w.label.Load(); p != nil {
		label = *p
	}

	// Grow the buffer until the full dump (all goroutines) fits.
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}

	slog.Error("UI event loop appears stuck; dumping goroutine stacks",
		"in_flight", label, "elapsed", elapsed.String())

	if w.dumpDir == "" {
		return
	}
	if err := os.MkdirAll(w.dumpDir, 0o755); err != nil {
		slog.Error("Failed to create stall dump directory", "error", err)
		return
	}
	path := filepath.Join(w.dumpDir,
		fmt.Sprintf("crush-stall-%s.txt", time.Now().Format("20060102-150405")))
	f, err := os.Create(path)
	if err != nil {
		slog.Error("Failed to write stall dump", "error", err)
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "Crush UI stall detected\nTime: %s\nIn-flight: %s\nElapsed: %s\n\n",
		time.Now().Format(time.RFC3339), label, elapsed)
	_, _ = f.Write(buf)
	slog.Error("Wrote stall goroutine dump", "path", path)
}
