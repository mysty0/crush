package cmd

import (
	"fmt"

	"github.com/charmbracelet/crush/internal/tmux"
	"github.com/spf13/cobra"
)

// tmuxResolveCmd is a hidden helper command intended for use by
// external tooling (e.g. a tmux-resurrect restore hook script), not by
// end users directly. Given the current working directory, it prints
// the ID of the Crush session that was last active in this exact tmux
// pane, if one was recorded and the pane's cwd still matches. It
// exits non-zero and prints nothing if there is no match, so callers
// can fall back to starting a plain new session.
var tmuxResolveCmd = &cobra.Command{
	Use:    "tmux-resolve",
	Short:  "Resolve the Crush session last active in the current tmux pane",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if !tmux.Available() {
			return fmt.Errorf("not running inside tmux")
		}

		// Read the pane's directory from tmux itself rather than
		// os.Getwd(): during a tmux-resurrect restore this command runs
		// in a freshly spawned shell whose cwd may not have settled yet,
		// and #{pane_current_path} is the authoritative directory tmux
		// (re)created the pane with.
		cwd, err := tmux.PaneCurrentPath(cmd.Context())
		if err != nil {
			return err
		}

		entry, ok, err := tmux.ResolveSession(cmd.Context(), cwd)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no session recorded for this tmux pane")
		}

		fmt.Fprintln(cmd.OutOrStdout(), entry.SessionID)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(tmuxResolveCmd)
}
