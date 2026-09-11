package shell

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// trackJob inserts a synthetic job into the manager without running a
// real shell. completedAgo of 0 means the job is still running;
// otherwise it finished that long ago.
func trackJob(m *BackgroundShellManager, id string, completedAgo time.Duration) {
	bs := &BackgroundShell{
		ID:        id,
		startedAt: time.Now().Add(-time.Hour),
		stdout:    &syncBuffer{},
		stderr:    &syncBuffer{},
		done:      make(chan struct{}),
		cancel:    func() {},
	}
	if completedAgo > 0 {
		bs.completedAt.Store(time.Now().Add(-completedAgo).Unix())
		close(bs.done)
	}
	m.shells.Set(id, bs)
}

// TestFinishedJobsDoNotConsumeJobSlots pins the meaning of
// MaxBackgroundJobs: it caps concurrently *running* jobs. Finished jobs
// stay tracked for hours so their output remains retrievable, and used
// to count against the same limit — so a long session would refuse to
// start new commands with nothing actually running, and the "terminate
// or wait for some jobs to complete" advice was impossible to act on.
func TestFinishedJobsDoNotConsumeJobSlots(t *testing.T) {
	t.Parallel()

	m := newBackgroundShellManager()
	for i := range MaxBackgroundJobs + 10 {
		trackJob(m, fmt.Sprintf("done-%d", i), time.Minute)
	}

	require.Zero(t, m.runningCount())
	_, err := m.Start(t.Context(), "", t.TempDir(), nil, nil, "true", "")
	require.NoError(t, err, "finished jobs must not block new work")
}

// TestRunningJobsStillHitTheLimit keeps the cap meaningful: genuinely
// concurrent work is still bounded.
func TestRunningJobsStillHitTheLimit(t *testing.T) {
	t.Parallel()

	m := newBackgroundShellManager()
	for i := range MaxBackgroundJobs {
		trackJob(m, fmt.Sprintf("run-%d", i), 0)
	}

	require.Equal(t, MaxBackgroundJobs, m.runningCount())
	_, err := m.Start(t.Context(), "", t.TempDir(), nil, nil, "true", "")
	require.ErrorContains(t, err, "maximum number of concurrent background jobs")
}

// TestCompletedJobsAreBoundedByCount checks that retained output cannot
// grow without bound in a long session: past MaxCompletedJobs, the
// oldest-completed jobs are evicted first while running jobs are left
// alone.
func TestCompletedJobsAreBoundedByCount(t *testing.T) {
	t.Parallel()

	m := newBackgroundShellManager()
	trackJob(m, "running", 0)
	// Oldest first, so "done-0" is the most stale.
	for i := range MaxCompletedJobs + 5 {
		trackJob(m, fmt.Sprintf("done-%d", i), time.Duration(MaxCompletedJobs+5-i)*time.Minute)
	}

	require.Equal(t, 5, m.evictCompletedOverflow())

	for i := range 5 {
		_, ok := m.Get(fmt.Sprintf("done-%d", i))
		require.False(t, ok, "oldest-completed jobs should be evicted first")
	}
	_, ok := m.Get(fmt.Sprintf("done-%d", MaxCompletedJobs+4))
	require.True(t, ok, "most recently completed job must be retained")
	_, ok = m.Get("running")
	require.True(t, ok, "eviction must never touch a running job")
}

// TestCompletedJobsUnderCapAreKept guards against over-eager eviction:
// nothing is dropped while retained output fits within the cap.
func TestCompletedJobsUnderCapAreKept(t *testing.T) {
	t.Parallel()

	m := newBackgroundShellManager()
	for i := range MaxCompletedJobs {
		trackJob(m, fmt.Sprintf("done-%d", i), time.Minute)
	}

	require.Zero(t, m.evictCompletedOverflow())
	require.Len(t, m.List(), MaxCompletedJobs)
}
