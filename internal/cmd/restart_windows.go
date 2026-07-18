//go:build windows

package cmd

import "errors"

// restartArgs carries the flag values needed to relaunch crush. See the
// Unix implementation (restart_other.go) for field meaning.
type restartArgs struct {
	Debug     bool
	Host      string
	DataDir   string
	Yolo      bool
	SessionID string
}

// restartProcess is not yet implemented on Windows: there is no exec()
// equivalent that replaces the running process image in place, so a
// Windows restart would need to spawn a new detached process and exit
// this one instead. The "Restart Crush" command is hidden on Windows
// (see internal/ui/dialog/commands.go) until that lands, so this should
// never actually be reached.
func restartProcess(restartArgs) error {
	return errors.New("restart is not yet supported on Windows")
}
