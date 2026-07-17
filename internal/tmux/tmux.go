// Package tmux provides minimal integration with tmux, letting Crush
// expose its active session to external tooling (e.g. a tmux-resurrect
// restore hook) both live, via pane-scoped user options, and durably,
// via a small on-disk mapping file that survives a full tmux server
// restart (pane user options do not).
package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
)

// paneKeyMaxRetries and paneKeyRetryDelay bound the retry loop in PaneKey.
// Four retries at 150ms cover roughly the first second after the tmux
// server (re)starts -- enough to ride out a restore-time burst -- without
// making a single resolve attempt noticeably slow to the user in the
// common (non-restore) case, where the first attempt always succeeds.
var (
	paneKeyMaxRetries = 4
	paneKeyRetryDelay = 150 * time.Millisecond
)

// errEmptyPaneKey is used internally when "tmux display-message" exits
// successfully but prints nothing, which is treated the same as a hard
// error for retry purposes.
var errEmptyPaneKey = errors.New("tmux: empty pane key")

// SessionIDOption is the tmux pane user option that holds the ID of the
// Crush session currently active in that pane.
const SessionIDOption = "@crush_session_id"

// SessionTitleOption is the tmux pane user option that holds the title
// of the Crush session currently active in that pane.
const SessionTitleOption = "@crush_session_title"

// mappingFile is the name of the on-disk pane-to-session mapping file,
// stored under the global Crush cache directory.
const mappingFile = "tmux-panes.json"

// Available reports whether Crush is running inside a tmux session.
func Available() bool {
	return os.Getenv("TMUX") != ""
}

// paneTarget returns the tmux target for the pane this process is
// running in, read from $TMUX_PANE. Unattached tmux/psmux commands
// (i.e. commands not run through a live client) do not reliably
// default -t to the calling pane: some implementations resolve the
// "current pane" via server-side focus tracking instead, which can
// point at a different pane than the one whose shell spawned the
// command. Passing -t explicitly makes targeting deterministic
// regardless of implementation.
func paneTarget() string {
	return os.Getenv("TMUX_PANE")
}

// PaneEntry records which Crush session was last active in a given
// tmux pane, along with the working directory it was running in. The
// working directory is used to sanity-check a lookup: if a pane's
// index gets reused for an unrelated project after a restart, the cwd
// mismatch lets callers fall back to starting a fresh session instead
// of resuming the wrong one.
type PaneEntry struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Cwd       string `json:"cwd"`
	UpdatedAt int64  `json:"updated_at"`
}

// SetSessionID sets the pane-local @crush_session_id user option to id.
// This is a live signal only: it does not survive a tmux server
// restart. Use RecordSession for durable state.
func SetSessionID(ctx context.Context, id string) error {
	return setPaneOption(ctx, SessionIDOption, id)
}

// SetSessionTitle sets the pane-local @crush_session_title user option
// to title. This is a live signal only: it does not survive a tmux
// server restart. Use RecordSession for durable state.
func SetSessionTitle(ctx context.Context, title string) error {
	return setPaneOption(ctx, SessionTitleOption, title)
}

func setPaneOption(ctx context.Context, key, value string) error {
	args := []string{"set-option", "-p"}
	if t := paneTarget(); t != "" {
		args = append(args, "-t", t)
	}
	args = append(args, key, value)
	cmd := exec.CommandContext(ctx, "tmux", args...)
	return cmd.Run()
}

// PaneKey returns a stable identifier for the current tmux pane, in
// the form "session_name:window_index.pane_index". It is used as the
// key into the on-disk pane-to-session mapping.
func PaneKey(ctx context.Context) (string, error) {
	return displayMessage(ctx, "#{session_name}:#{window_index}.#{pane_index}")
}

// PaneCurrentPath returns the working directory tmux tracks for the
// current pane (its "#{pane_current_path}"). This is the authoritative
// source for a pane's directory during a tmux-resurrect restore -- the
// pane is (re)created with that directory via "new-window -c" -- whereas
// os.Getwd() of a process launched into the pane can momentarily report
// the wrong directory (e.g. $HOME) before the shell has settled, which
// would defeat the cwd guard in ResolveSession.
func PaneCurrentPath(ctx context.Context) (string, error) {
	return displayMessage(ctx, "#{pane_current_path}")
}

// displayMessage runs "tmux display-message -p <format>" against the
// current pane and returns the trimmed output.
//
// The call is retried a few times with a short delay: it sits on the
// critical path of a tmux-resurrect restore hook (see cmd/tmux-resolve),
// where dozens of panes can each shell out to tmux in a tight burst moments
// after the server itself finished (re)starting. A single transient hiccup
// on the control socket under that burst would otherwise permanently fail
// the resolve for that pane -- the caller has no later opportunity to retry
// -- so it lands the user on a blank session instead of the one that was
// actually running there.
func displayMessage(ctx context.Context, format string) (string, error) {
	args := []string{"display-message", "-p"}
	if t := paneTarget(); t != "" {
		args = append(args, "-t", t)
	}
	args = append(args, format)

	var lastErr error
	for attempt := 0; attempt <= paneKeyMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(paneKeyRetryDelay):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		// A fresh exec.Cmd is required each attempt: exec.Cmd cannot be
		// re-run once Output has been called.
		out, err := exec.CommandContext(ctx, "tmux", args...).Output()
		if v := strings.TrimSpace(string(out)); err == nil && v != "" {
			return v, nil
		}
		lastErr = err
		if lastErr == nil {
			lastErr = errEmptyPaneKey
		}
	}
	return "", lastErr
}

// RecordSession durably associates the current tmux pane with the
// given Crush session, so it can be resolved again after a full tmux
// server restart (e.g. via tmux-resurrect). It is a no-op outside
// tmux.
func RecordSession(ctx context.Context, cwd, sessionID, title string) error {
	key, err := PaneKey(ctx)
	if err != nil {
		return err
	}
	return updateMapping(func(m map[string]PaneEntry) {
		m[key] = PaneEntry{
			SessionID: sessionID,
			Title:     title,
			Cwd:       cwd,
			UpdatedAt: time.Now().Unix(),
		}
	})
}

// ResolveSession looks up the Crush session previously recorded for
// the current tmux pane. It returns ok=false if there is no recorded
// entry, or if the entry's working directory does not match cwd
// (guarding against a restored pane landing on a stale mapping from an
// unrelated project).
func ResolveSession(ctx context.Context, cwd string) (entry PaneEntry, ok bool, err error) {
	key, err := PaneKey(ctx)
	if err != nil {
		return PaneEntry{}, false, err
	}
	m, err := readMapping()
	if err != nil {
		return PaneEntry{}, false, err
	}
	e, found := m[key]
	if !found || !sameDir(e.Cwd, cwd) {
		return PaneEntry{}, false, nil
	}
	return e, true, nil
}

// sameDir reports whether two directory paths refer to the same
// location. It compares them cleaned, then falls back to comparing
// their symlink-resolved forms. The fallback matters because the cwd
// recorded by a running Crush (RecordSession) may be unresolved, while
// the value read back from tmux's "#{pane_current_path}" is typically
// symlink-resolved; without it, a project reached through a symlink
// would spuriously fail the cwd guard and land on a blank session.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	ra, err := filepath.EvalSymlinks(ca)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(cb)
	if err != nil {
		return false
	}
	return ra == rb
}

func mappingPath() string {
	return filepath.Join(config.GlobalCacheDir(), mappingFile)
}

func readMapping() (map[string]PaneEntry, error) {
	data, err := os.ReadFile(mappingPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]PaneEntry{}, nil
		}
		return nil, err
	}
	var m map[string]PaneEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]PaneEntry{}, nil
	}
	if m == nil {
		m = map[string]PaneEntry{}
	}
	return m, nil
}

// updateMapping reads the mapping file, applies fn, and writes it back
// atomically. Callers race benignly: the last writer wins, which is
// acceptable since entries are pane-scoped and only one Crush process
// normally writes a given pane's key at a time.
func updateMapping(fn func(map[string]PaneEntry)) error {
	dir := config.GlobalCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m, err := readMapping()
	if err != nil {
		return err
	}
	fn(m)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := mappingPath()
	tmp, err := os.CreateTemp(dir, mappingFile+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
