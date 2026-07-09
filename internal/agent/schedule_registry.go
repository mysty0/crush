package agent

import (
	"context"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
)

// ScheduleKind distinguishes the two ways a scheduled task can be
// paced, mirroring Claude Code's split between a fixed-cadence cron
// task and a self-paced wakeup loop:
//
//   - ScheduleKindCron fires on a fixed interval chosen once at
//     creation. The scheduler re-arms it automatically; the fired
//     turn has no say over timing.
//   - ScheduleKindWakeup fires once, then waits for the fired turn to
//     call ScheduleWakeup again with a new delay (or stop it). If it
//     doesn't, one fallback firing is given as a reminder before the
//     task stops itself.
type ScheduleKind string

const (
	ScheduleKindCron   ScheduleKind = "cron"
	ScheduleKindWakeup ScheduleKind = "wakeup"
)

// ScheduleState is a scheduled task's lifecycle state.
type ScheduleState string

const (
	// ScheduleActive means the task is still waiting for (or
	// dispatching) its next firing.
	ScheduleActive ScheduleState = "active"
	// ScheduleStopped means the task will never fire again (canceled,
	// reached max_runs, expired, or gave up after a missed wakeup
	// reschedule).
	ScheduleStopped ScheduleState = "stopped"
)

// ScheduledTaskStatus is a snapshot of one scheduled task, returned by
// ScheduleList and (later) consumable by a UI the same way
// WorkflowStatus is.
type ScheduledTaskStatus struct {
	// ID identifies the task for ScheduleWakeup (reschedule) and
	// ScheduleCancel calls.
	ID string
	// OriginSessionID is the session the task was created from, and
	// the session its firings run in and report back to.
	OriginSessionID string
	Kind            ScheduleKind
	// Prompt is the text sent into the origin session on each firing.
	// For wakeup tasks, a reschedule call can replace it.
	Prompt string
	// IntervalSeconds is the fixed cadence for a cron task, or the
	// most recently requested delay for a wakeup task (informational
	// only once running).
	IntervalSeconds int
	// MaxRuns caps the number of firings; 0 means unbounded (subject
	// to ExpiresAt).
	MaxRuns    int
	RunCount   int
	NextFireAt time.Time
	CreatedAt  time.Time
	// ExpiresAt is a hard lifetime cap so a forgotten task cannot run
	// forever.
	ExpiresAt time.Time
	State     ScheduleState
	// StopReason explains why State is Stopped (e.g. "canceled",
	// "reached max_runs", "expired", "no reschedule after fallback
	// wakeup"). Empty while active.
	StopReason string
	// LastResult is a short, best-effort note about the most recent
	// firing's outcome (e.g. "ran", "queued", "error: ...").
	LastResult string
}

// wakeupRequest carries a ScheduleWakeup(task_id=...) call's requested
// next delay (and optionally updated prompt) from the tool handler to
// the task's scheduler goroutine.
type wakeupRequest struct {
	delaySeconds int
	prompt       string
}

// runningSchedule is the registry's mutable, internal record for one
// scheduled task.
type runningSchedule struct {
	status ScheduledTaskStatus
	cancel context.CancelFunc
	// reschedule delivers a pending ScheduleWakeup reschedule request
	// to the scheduler goroutine waiting in fireScheduledTask.
	// Buffered size 1 so the tool call handler never blocks on it.
	reschedule chan wakeupRequest
	// missedReschedule is set once a wakeup firing's fallback window
	// elapsed without a reschedule request. A second consecutive miss
	// stops the task instead of firing again.
	missedReschedule bool
}

// scheduleRegistry tracks every scheduled task (cron or wakeup) known
// to a coordinator, keyed by task ID.
type scheduleRegistry struct {
	entries *csync.Map[string, *runningSchedule]
}

func newScheduleRegistry() *scheduleRegistry {
	return &scheduleRegistry{entries: csync.NewMap[string, *runningSchedule]()}
}

// register adds a new scheduled task.
func (r *scheduleRegistry) register(status ScheduledTaskStatus, cancel context.CancelFunc) {
	r.entries.Set(status.ID, &runningSchedule{
		status:     status,
		cancel:     cancel,
		reschedule: make(chan wakeupRequest, 1),
	})
}

// get returns a snapshot of a task's status.
func (r *scheduleRegistry) get(id string) (ScheduledTaskStatus, bool) {
	e, ok := r.entries.Get(id)
	if !ok {
		return ScheduledTaskStatus{}, false
	}
	return e.status, true
}

// mutate applies fn to the task's entry under the map's own
// synchronization, storing the result back. No-op if the task is not
// registered.
func (r *scheduleRegistry) mutate(id string, fn func(e *runningSchedule)) {
	e, ok := r.entries.Get(id)
	if !ok {
		return
	}
	fn(e)
	r.entries.Set(id, e)
}

// setNextFire updates when a task should fire next.
func (r *scheduleRegistry) setNextFire(id string, t time.Time) {
	r.mutate(id, func(e *runningSchedule) { e.status.NextFireAt = t })
}

// recordRun increments a task's run count and records a short summary
// of its most recent firing.
func (r *scheduleRegistry) recordRun(id, result string) {
	r.mutate(id, func(e *runningSchedule) {
		e.status.RunCount++
		e.status.LastResult = result
	})
}

// stop marks a task stopped and cancels its scheduler goroutine. No-op
// if the task is unknown or already stopped, so a second stop (e.g. the
// UI canceling a task that just reached max_runs on its own) can never
// clobber the original StopReason.
func (r *scheduleRegistry) stop(id, reason string) {
	e, ok := r.entries.Get(id)
	if !ok || e.status.State != ScheduleActive {
		return
	}
	e.status.State = ScheduleStopped
	e.status.StopReason = reason
	r.entries.Set(id, e)
	if e.cancel != nil {
		e.cancel()
	}
}

// requestReschedule delivers a ScheduleWakeup(task_id=...) call's
// requested next delay/prompt to the task's scheduler goroutine.
// Returns false if the task doesn't exist or has already stopped.
func (r *scheduleRegistry) requestReschedule(id string, delaySeconds int, prompt string) bool {
	e, ok := r.entries.Get(id)
	if !ok || e.status.State != ScheduleActive {
		return false
	}
	req := wakeupRequest{delaySeconds: delaySeconds, prompt: prompt}
	select {
	case e.reschedule <- req:
	default:
		// A reschedule is already pending (e.g. called twice before
		// the scheduler drained the first); replace it with the
		// latest request rather than blocking.
		select {
		case <-e.reschedule:
		default:
		}
		e.reschedule <- req
	}
	return true
}

// rescheduleChan returns the task's reschedule channel for the
// scheduler goroutine to select on, or nil if the task is unknown (a
// nil channel blocks forever in a select, which is the safe fallback
// here).
func (r *scheduleRegistry) rescheduleChan(id string) chan wakeupRequest {
	e, ok := r.entries.Get(id)
	if !ok {
		return nil
	}
	return e.reschedule
}

// awaitWakeupReschedule waits up to fallback for a reschedule request
// on taskID (delivered by a ScheduleWakeup tool call naming this task)
// and applies the resulting state transition:
//
//   - A request arrives: the task is rescheduled to the requested
//     delay/prompt and its missed-reschedule flag is cleared.
//   - fallback elapses first, and this is the task's first missed
//     reschedule: one fallback firing is scheduled immediately as a
//     reminder (NextFireAt set to now), and the missed flag is set.
//   - fallback elapses first, and the missed flag was already set
//     (i.e. the fallback firing above also went unanswered): the task
//     is stopped.
//   - ctx is done first: no state change.
//
// Returns once a decision has been applied.
func (r *scheduleRegistry) awaitWakeupReschedule(ctx context.Context, taskID string, fallback time.Duration) {
	select {
	case req := <-r.rescheduleChan(taskID):
		r.mutate(taskID, func(e *runningSchedule) {
			e.status.NextFireAt = time.Now().Add(time.Duration(req.delaySeconds) * time.Second)
			e.status.IntervalSeconds = req.delaySeconds
			if req.prompt != "" {
				e.status.Prompt = req.prompt
			}
			e.missedReschedule = false
		})
	case <-time.After(fallback):
		if alreadyMissed := r.markMissedReschedule(taskID); alreadyMissed {
			r.stop(taskID, "no reschedule after fallback wakeup")
			return
		}
		// Give one fallback firing right away as a reminder; if that
		// one also goes unanswered, the task stops on the next miss.
		r.setNextFire(taskID, time.Now())
	case <-ctx.Done():
	}
}

// markMissedReschedule flips a task's missed-reschedule flag and
// reports whether it was already set (i.e. this is the second
// consecutive miss).
func (r *scheduleRegistry) markMissedReschedule(id string) (alreadyMissed bool) {
	e, ok := r.entries.Get(id)
	if !ok {
		return true
	}
	alreadyMissed = e.missedReschedule
	e.missedReschedule = true
	r.entries.Set(id, e)
	return alreadyMissed
}

// clearMissedReschedule resets a task's missed-reschedule flag, called
// whenever a reschedule request actually arrives.
func (r *scheduleRegistry) clearMissedReschedule(id string) {
	r.mutate(id, func(e *runningSchedule) { e.missedReschedule = false })
}

// countActive returns how many active tasks originated from
// originSessionID, used to cap concurrent tasks per session.
func (r *scheduleRegistry) countActive(originSessionID string) int {
	n := 0
	for e := range r.entries.Seq() {
		if e.status.OriginSessionID == originSessionID && e.status.State == ScheduleActive {
			n++
		}
	}
	return n
}

// list returns every task that originated from originSessionID.
func (r *scheduleRegistry) list(originSessionID string) []ScheduledTaskStatus {
	var out []ScheduledTaskStatus
	for e := range r.entries.Seq() {
		if e.status.OriginSessionID == originSessionID {
			out = append(out, e.status)
		}
	}
	return out
}

// listAll returns every task known to the registry, regardless of
// origin session (for a future cross-session UI).
func (r *scheduleRegistry) listAll() []ScheduledTaskStatus {
	var out []ScheduledTaskStatus
	for e := range r.entries.Seq() {
		out = append(out, e.status)
	}
	return out
}

// remove deletes a stopped task from the registry so it no longer
// appears in ScheduleList or the UI picker. Called after a task's
// terminal state has lingered briefly (see scheduleLingerAfterStop).
func (r *scheduleRegistry) remove(id string) {
	r.entries.Del(id)
}
