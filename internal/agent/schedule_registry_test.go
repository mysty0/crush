package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSchedule(id string) (ScheduledTaskStatus, context.CancelFunc, *bool) {
	canceled := false
	status := ScheduledTaskStatus{
		ID:              id,
		OriginSessionID: "session-1",
		Kind:            ScheduleKindCron,
		Prompt:          "check things",
		IntervalSeconds: 60,
		CreatedAt:       time.Now(),
		ExpiresAt:       time.Now().Add(time.Hour),
		State:           ScheduleActive,
	}
	cancel := func() { canceled = true }
	return status, cancel, &canceled
}

func TestScheduleRegistry_RegisterAndGet(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()

	status, cancel, _ := newTestSchedule("cron-1")
	r.register(status, cancel)

	got, ok := r.get("cron-1")
	require.True(t, ok)
	assert.Equal(t, status, got)

	_, ok = r.get("does-not-exist")
	assert.False(t, ok)
}

func TestScheduleRegistry_SetNextFireAndRecordRun(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, _ := newTestSchedule("cron-1")
	r.register(status, cancel)

	next := time.Now().Add(5 * time.Minute)
	r.setNextFire("cron-1", next)
	got, _ := r.get("cron-1")
	assert.WithinDuration(t, next, got.NextFireAt, time.Millisecond)

	r.recordRun("cron-1", "ran")
	got, _ = r.get("cron-1")
	assert.Equal(t, 1, got.RunCount)
	assert.Equal(t, "ran", got.LastResult)

	r.recordRun("cron-1", "ran again")
	got, _ = r.get("cron-1")
	assert.Equal(t, 2, got.RunCount)
	assert.Equal(t, "ran again", got.LastResult)
}

func TestScheduleRegistry_Stop(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, canceled := newTestSchedule("cron-1")
	r.register(status, cancel)

	r.stop("cron-1", "canceled")

	got, ok := r.get("cron-1")
	require.True(t, ok)
	assert.Equal(t, ScheduleStopped, got.State)
	assert.Equal(t, "canceled", got.StopReason)
	assert.True(t, *canceled, "stop should invoke the task's cancel func")

	// Stopping an unknown task is a no-op, not a panic.
	r.stop("does-not-exist", "canceled")
}

func TestScheduleRegistry_StopIsIdempotent(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, _ := newTestSchedule("cron-1")
	r.register(status, cancel)

	r.stop("cron-1", "reached max_runs")
	// A later stop (e.g. the user canceling from the UI a task that
	// already reached max_runs on its own) must not overwrite the
	// original reason.
	r.stop("cron-1", "canceled")

	got, ok := r.get("cron-1")
	require.True(t, ok)
	assert.Equal(t, "reached max_runs", got.StopReason)
}

func TestScheduleRegistry_RequestReschedule(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, _ := newTestSchedule("wake-1")
	status.Kind = ScheduleKindWakeup
	r.register(status, cancel)

	ok := r.requestReschedule("wake-1", 120, "new prompt")
	require.True(t, ok, "rescheduling an active task should succeed")

	select {
	case req := <-r.rescheduleChan("wake-1"):
		assert.Equal(t, 120, req.delaySeconds)
		assert.Equal(t, "new prompt", req.prompt)
	default:
		t.Fatal("expected a pending reschedule request")
	}

	// A second reschedule before the first is drained replaces it
	// rather than blocking.
	require.True(t, r.requestReschedule("wake-1", 30, "a"))
	require.True(t, r.requestReschedule("wake-1", 60, "b"))
	select {
	case req := <-r.rescheduleChan("wake-1"):
		assert.Equal(t, 60, req.delaySeconds)
		assert.Equal(t, "b", req.prompt)
	default:
		t.Fatal("expected the latest pending reschedule request")
	}

	// Unknown or stopped tasks refuse a reschedule.
	assert.False(t, r.requestReschedule("does-not-exist", 60, "x"))
	r.stop("wake-1", "canceled")
	assert.False(t, r.requestReschedule("wake-1", 60, "x"))
}

func TestScheduleRegistry_RescheduleChanUnknownTask(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	assert.Nil(t, r.rescheduleChan("does-not-exist"))
}

func TestScheduleRegistry_MissedReschedule(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, _ := newTestSchedule("wake-1")
	status.Kind = ScheduleKindWakeup
	r.register(status, cancel)

	// First miss: not already missed.
	alreadyMissed := r.markMissedReschedule("wake-1")
	assert.False(t, alreadyMissed)

	// Second consecutive miss: already missed.
	alreadyMissed = r.markMissedReschedule("wake-1")
	assert.True(t, alreadyMissed)

	// Clearing resets the flag so the next miss is treated as the first again.
	r.clearMissedReschedule("wake-1")
	alreadyMissed = r.markMissedReschedule("wake-1")
	assert.False(t, alreadyMissed)

	// An unknown task reports "already missed" defensively so callers
	// stop rather than loop forever on a vanished entry.
	assert.True(t, r.markMissedReschedule("does-not-exist"))
}

func TestScheduleRegistry_CountActiveAndList(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()

	s1, c1, _ := newTestSchedule("cron-1")
	s1.OriginSessionID = "session-a"
	r.register(s1, c1)

	s2, c2, _ := newTestSchedule("cron-2")
	s2.OriginSessionID = "session-a"
	r.register(s2, c2)

	s3, c3, _ := newTestSchedule("cron-3")
	s3.OriginSessionID = "session-b"
	r.register(s3, c3)

	assert.Equal(t, 2, r.countActive("session-a"))
	assert.Equal(t, 1, r.countActive("session-b"))
	assert.Equal(t, 0, r.countActive("session-nonexistent"))

	r.stop("cron-1", "canceled")
	assert.Equal(t, 1, r.countActive("session-a"), "a stopped task no longer counts as active")

	listA := r.list("session-a")
	assert.Len(t, listA, 2, "list includes stopped tasks, unlike countActive")

	all := r.listAll()
	assert.Len(t, all, 3)
}

func TestScheduleRegistry_MutateNoopOnUnknownTask(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	called := false
	r.mutate("does-not-exist", func(e *runningSchedule) { called = true })
	assert.False(t, called, "mutate should be a no-op for an unregistered task")
}

func TestScheduleRegistry_AwaitWakeupReschedule_RequestArrives(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, _ := newTestSchedule("wake-1")
	status.Kind = ScheduleKindWakeup
	r.register(status, cancel)

	// Deliver a reschedule request slightly before the fallback fires.
	go func() {
		time.Sleep(5 * time.Millisecond)
		r.requestReschedule("wake-1", 90, "updated prompt")
	}()

	r.awaitWakeupReschedule(context.Background(), "wake-1", 50*time.Millisecond)

	got, ok := r.get("wake-1")
	require.True(t, ok)
	assert.Equal(t, ScheduleActive, got.State, "a successful reschedule keeps the task active")
	assert.Equal(t, "updated prompt", got.Prompt)
	assert.Equal(t, 90, got.IntervalSeconds)
	assert.WithinDuration(t, time.Now().Add(90*time.Second), got.NextFireAt, 2*time.Second)
}

func TestScheduleRegistry_AwaitWakeupReschedule_FirstMissGetsFallback(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, _ := newTestSchedule("wake-1")
	status.Kind = ScheduleKindWakeup
	r.register(status, cancel)

	r.awaitWakeupReschedule(context.Background(), "wake-1", 5*time.Millisecond)

	got, ok := r.get("wake-1")
	require.True(t, ok)
	assert.Equal(t, ScheduleActive, got.State, "the first missed reschedule gets one fallback firing, not a stop")
	assert.WithinDuration(t, time.Now(), got.NextFireAt, 2*time.Second, "the fallback firing is scheduled immediately")

	e, ok := r.entries.Get("wake-1")
	require.True(t, ok)
	assert.True(t, e.missedReschedule)
}

func TestScheduleRegistry_AwaitWakeupReschedule_SecondMissStops(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, canceled := newTestSchedule("wake-1")
	status.Kind = ScheduleKindWakeup
	r.register(status, cancel)

	// First miss: gets the fallback firing.
	r.awaitWakeupReschedule(context.Background(), "wake-1", 5*time.Millisecond)
	// Second consecutive miss: stops the task.
	r.awaitWakeupReschedule(context.Background(), "wake-1", 5*time.Millisecond)

	got, ok := r.get("wake-1")
	require.True(t, ok)
	assert.Equal(t, ScheduleStopped, got.State)
	assert.Equal(t, "no reschedule after fallback wakeup", got.StopReason)
	assert.True(t, *canceled)
}

func TestScheduleRegistry_AwaitWakeupReschedule_RescheduleClearsMissedFlag(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, _ := newTestSchedule("wake-1")
	status.Kind = ScheduleKindWakeup
	r.register(status, cancel)

	// First miss sets the flag.
	r.awaitWakeupReschedule(context.Background(), "wake-1", 5*time.Millisecond)
	e, _ := r.entries.Get("wake-1")
	require.True(t, e.missedReschedule)

	// A reschedule that arrives after that should clear it, so a
	// later miss is treated as a fresh first miss rather than
	// immediately stopping the task.
	go func() {
		time.Sleep(5 * time.Millisecond)
		r.requestReschedule("wake-1", 60, "")
	}()
	r.awaitWakeupReschedule(context.Background(), "wake-1", 50*time.Millisecond)

	e, _ = r.entries.Get("wake-1")
	assert.False(t, e.missedReschedule)
}

func TestScheduleRegistry_AwaitWakeupReschedule_ContextDone(t *testing.T) {
	t.Parallel()
	r := newScheduleRegistry()
	status, cancel, _ := newTestSchedule("wake-1")
	status.Kind = ScheduleKindWakeup
	r.register(status, cancel)

	ctx, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()

	r.awaitWakeupReschedule(ctx, "wake-1", time.Hour)

	got, ok := r.get("wake-1")
	require.True(t, ok)
	assert.Equal(t, ScheduleActive, got.State, "a canceled context should not itself change task state")
}
