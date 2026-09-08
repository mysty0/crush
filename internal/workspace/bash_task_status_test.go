package workspace

import (
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestBashJobTaskStatusCarriesFinishTime guards the wiring the task
// picker relies on to age finished bash jobs out of the list: a
// completed job must carry its finish time through the projection.
// Without it every finished job looks like it has no known end and
// lingers in the UI for as long as the shell manager retains it, which
// is hours.
func TestBashJobTaskStatusCarriesFinishTime(t *testing.T) {
	t.Parallel()

	completed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	t.Run("finished job carries its finish time", func(t *testing.T) {
		t.Parallel()
		got := bashJobTaskStatus(shell.BackgroundJobStatus{
			ID:          "job-1",
			SessionID:   "s-1",
			Description: "Wait for sub-agents",
			StartedAt:   completed.Add(-time.Minute),
			Done:        true,
			CompletedAt: completed,
		})
		require.Equal(t, agent.TaskDone, got.State)
		require.Equal(t, completed, got.FinishedAt,
			"finish time must survive the projection")
	})

	t.Run("failed job carries its finish time", func(t *testing.T) {
		t.Parallel()
		got := bashJobTaskStatus(shell.BackgroundJobStatus{
			ID:          "job-2",
			Done:        true,
			Err:         errors.New("boom"),
			CompletedAt: completed,
		})
		require.Equal(t, agent.TaskFailed, got.State)
		require.Equal(t, completed, got.FinishedAt)
	})

	t.Run("running job has no finish time", func(t *testing.T) {
		t.Parallel()
		got := bashJobTaskStatus(shell.BackgroundJobStatus{
			ID:        "job-3",
			StartedAt: time.Now(),
		})
		require.Equal(t, agent.TaskRunning, got.State)
		require.True(t, got.FinishedAt.IsZero())
	})

	t.Run("label falls back to the command", func(t *testing.T) {
		t.Parallel()
		got := bashJobTaskStatus(shell.BackgroundJobStatus{
			ID:      "job-4",
			Command: "sleep 270",
		})
		require.Equal(t, "sleep 270", got.Label)
	})
}
