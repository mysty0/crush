//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// restartArgs carries the flag values needed to relaunch crush landing
// back in the same place: the same session (SessionID), and the same
// debug/host/data-dir/yolo settings the original process was started
// with. The working directory is not included -- it is inherited from
// the current process automatically (syscall.Exec never changes cwd),
// and by the time a restart is requested any --cwd flag from the
// original invocation has already been applied via os.Chdir.
type restartArgs struct {
	Debug     bool
	Host      string
	DataDir   string
	Yolo      bool
	SessionID string
}

// restartProcess replaces the current process image with a fresh
// invocation of the same executable (Unix exec: same PID, no parent/child
// handoff), so a rebuilt or updated binary on disk takes effect
// immediately. Only ever called after the Bubble Tea program has returned
// from Run and the terminal has been restored to cooked mode.
func restartProcess(a restartArgs) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}
	exe = resolveRestartExecutable(exe)
	args := buildRestartArgs(exe, a)
	return syscall.Exec(exe, args, os.Environ())
}

// resolveRestartExecutable adjusts the path os.Executable reports so
// restart still targets the right file even if it was replaced by
// unlinking rather than truncating in place.
//
// On Linux, os.Executable reads /proc/self/exe. If the on-disk file was
// replaced by unlinking (e.g. `rm crush && mv crush.new crush` instead of
// `go build -o crush .` truncating the existing file), the still-running
// process's link target is reported with a " (deleted)" suffix. Strip it
// so restart still resolves to the new file now sitting at the same path.
func resolveRestartExecutable(exe string) string {
	return strings.TrimSuffix(exe, " (deleted)")
}

// buildRestartArgs builds the argv for restarting exe with the given
// flags, always as argv[0]=exe followed by only the flags that were
// actually set -- so an omitted/zero-value field round-trips to "not
// passed" rather than an explicit empty/false flag.
func buildRestartArgs(exe string, a restartArgs) []string {
	args := []string{exe}
	if a.Debug {
		args = append(args, "--debug")
	}
	if a.Host != "" {
		args = append(args, "--host", a.Host)
	}
	if a.DataDir != "" {
		args = append(args, "--data-dir", a.DataDir)
	}
	if a.Yolo {
		args = append(args, "--yolo")
	}
	if a.SessionID != "" {
		args = append(args, "--session", a.SessionID)
	}
	return args
}
