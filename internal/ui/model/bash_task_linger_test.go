package model

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/stretchr/testify/require"
)

// TestFinishedBashJobsAgeOutOfTaskList guards the task picker against
// filling up with finished work. Background bash jobs are retained in
// the shell manager for hours so their output stays retrievable via
// job_output, but a job that ended long ago should not keep occupying a
// row in the picker.
func TestFinishedBashJobsAgeOutOfTaskList(t *testing.T) {
	t.Parallel()

	bash := func(state agent.TaskState, finishedAt time.Time) agent.TaskStatus {
		return agent.TaskStatus{
			Ref:        agent.TaskRef{Kind: agent.TaskKindBash, ID: "job-1"},
			State:      state,
			FinishedAt: finishedAt,
		}
	}

	t.Run("running job is kept regardless of age", func(t *testing.T) {
		t.Parallel()
		task := bash(agent.TaskRunning, time.Time{})
		task.StartedAt = time.Now().Add(-24 * time.Hour)
		require.False(t, finishedBashHasAgedOut(task))
	})

	t.Run("just-finished job lingers", func(t *testing.T) {
		t.Parallel()
		require.False(t, finishedBashHasAgedOut(bash(agent.TaskDone, time.Now())))
	})

	t.Run("long-finished job ages out", func(t *testing.T) {
		t.Parallel()
		old := time.Now().Add(-finishedBashLinger - time.Minute)
		require.True(t, finishedBashHasAgedOut(bash(agent.TaskDone, old)))
	})

	t.Run("long-failed job ages out too", func(t *testing.T) {
		t.Parallel()
		old := time.Now().Add(-finishedBashLinger - time.Minute)
		require.True(t, finishedBashHasAgedOut(bash(agent.TaskFailed, old)))
	})

	t.Run("unknown finish time is kept", func(t *testing.T) {
		t.Parallel()
		// A finished job with no recorded timestamp must not vanish:
		// hiding work on missing data is worse than an extra row.
		require.False(t, finishedBashHasAgedOut(bash(agent.TaskDone, time.Time{})))
	})

	t.Run("other task kinds are unaffected", func(t *testing.T) {
		t.Parallel()
		old := time.Now().Add(-finishedBashLinger - time.Hour)
		for _, kind := range []agent.TaskKind{
			agent.TaskKindWorkflow,
			agent.TaskKindSchedule,
			agent.TaskKindSubAgent,
		} {
			task := agent.TaskStatus{
				Ref:        agent.TaskRef{Kind: kind, ID: "t"},
				State:      agent.TaskDone,
				FinishedAt: old,
			}
			require.False(t, finishedBashHasAgedOut(task),
				"only bash jobs age out here; %s has its own lifecycle", kind)
		}
	})
}
