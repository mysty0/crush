package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/permission"
)

const (
	// ScheduleCronToolName creates a fixed-interval recurring task.
	ScheduleCronToolName = "ScheduleCron"
	// ScheduleWakeupToolName starts (or reschedules) a self-paced
	// wakeup loop.
	ScheduleWakeupToolName = "ScheduleWakeup"
	// ScheduleListToolName lists the current session's scheduled
	// tasks.
	ScheduleListToolName = "ScheduleList"
	// ScheduleCancelToolName stops a scheduled task.
	ScheduleCancelToolName = "ScheduleCancel"
)

const (
	// scheduleMinIntervalSeconds is the minimum firing interval/delay
	// for either kind of task, to keep a misconfigured loop from
	// hammering the session.
	scheduleMinIntervalSeconds = 30
	// scheduleMaxTasksPerSession caps concurrent active tasks per
	// origin session so a runaway agent can't spawn unbounded
	// background loops.
	scheduleMaxTasksPerSession = 10
	// scheduleDefaultExpiry is the hard lifetime cap on any scheduled
	// task, so a forgotten loop cannot run forever.
	scheduleDefaultExpiry = 24 * time.Hour
	// scheduleWakeupFallback is how long a wakeup task waits for a
	// reschedule request before treating the firing as unanswered.
	// Mirrors Claude Code's /loop ~20-minute fallback wakeup.
	scheduleWakeupFallback = 20 * time.Minute
	// scheduleLingerAfterStop is how long a stopped task remains in
	// the registry (and thus ScheduleList / the UI picker) before
	// being cleared, mirroring workflowLingerAfterFinish.
	scheduleLingerAfterStop = 5 * time.Second
)

//go:embed templates/schedule_cron.md
var scheduleCronDescription string

//go:embed templates/schedule_wakeup.md
var scheduleWakeupDescription string

//go:embed templates/schedule_list.md
var scheduleListDescription string

//go:embed templates/schedule_cancel.md
var scheduleCancelDescription string

// ScheduleCronParams are the parameters for the ScheduleCron tool.
type ScheduleCronParams struct {
	Prompt          string `json:"prompt" description:"The prompt to run each time this task fires."`
	IntervalSeconds int    `json:"interval_seconds" description:"Fixed interval between firings, in seconds (minimum 30)."`
	MaxRuns         int    `json:"max_runs,omitempty" description:"Optional. Stop automatically after this many firings. Omit for unbounded (still subject to a 24h expiry)."`
}

// ScheduleWakeupParams are the parameters for the ScheduleWakeup tool.
type ScheduleWakeupParams struct {
	TaskID       string `json:"task_id,omitempty" description:"Omit to start a new self-paced loop. Set to an existing task's ID (returned when it was created, or from ScheduleList) to reschedule that loop's next firing instead of starting a new one."`
	Prompt       string `json:"prompt" description:"The prompt to run when this task fires. When rescheduling, this replaces the previous prompt."`
	DelaySeconds int    `json:"delay_seconds" description:"How long to wait before firing, in seconds (minimum 30). Choose this based on what you expect to have changed by then -- shorter while something is actively in progress, longer otherwise."`
}

// ScheduleListParams are the parameters for the ScheduleList tool (none).
type ScheduleListParams struct{}

// ScheduleCancelParams are the parameters for the ScheduleCancel tool.
type ScheduleCancelParams struct {
	TaskID string `json:"task_id" description:"The ID of the scheduled task to stop, as returned when it was created or listed."`
}

// scheduleCronTool implements the ScheduleCron tool: a fixed-interval
// recurring task the scheduler re-arms automatically, independent of
// what the fired turn does.
func (c *coordinator) scheduleCronTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		ScheduleCronToolName,
		scheduleCronDescription,
		func(ctx context.Context, params ScheduleCronParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			if params.IntervalSeconds < scheduleMinIntervalSeconds {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("interval_seconds must be at least %d", scheduleMinIntervalSeconds)), nil
			}
			if params.MaxRuns < 0 {
				return fantasy.NewTextErrorResponse("max_runs cannot be negative"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			if n := c.schedules.countActive(sessionID); n >= scheduleMaxTasksPerSession {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("too many active scheduled tasks (%d); cancel one with ScheduleCancel first", n)), nil
			}

			p, err := c.permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        c.cfg.WorkingDir(),
					ToolCallID:  call.ID,
					ToolName:    ScheduleCronToolName,
					Action:      "schedule",
					Description: fmt.Sprintf("Schedule a recurring task every %ds: %s", params.IntervalSeconds, params.Prompt),
					Params:      params,
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return tools.NewPermissionDeniedResponse(), nil
			}

			taskID := newScheduleID(ScheduleKindCron)
			now := time.Now()
			status := ScheduledTaskStatus{
				ID:              taskID,
				OriginSessionID: sessionID,
				Kind:            ScheduleKindCron,
				Prompt:          params.Prompt,
				IntervalSeconds: params.IntervalSeconds,
				MaxRuns:         params.MaxRuns,
				NextFireAt:      now.Add(time.Duration(params.IntervalSeconds) * time.Second),
				CreatedAt:       now,
				ExpiresAt:       now.Add(scheduleDefaultExpiry),
				State:           ScheduleActive,
			}
			// Detached from the calling turn's context so the task
			// keeps running after this turn ends; only Cancel/expiry/
			// max_runs stop it.
			bgCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			c.schedules.register(status, cancel)
			go c.runScheduledTask(bgCtx, taskID)

			return fantasy.NewTextResponse(fmt.Sprintf(
				"Scheduled recurring task %s: fires every %ds%s. It runs in the background and reports back into this session after each firing. Use ScheduleCancel(task_id=%q) to stop it.",
				taskID, params.IntervalSeconds, maxRunsSuffix(params.MaxRuns), taskID,
			)), nil
		},
	)
}

// scheduleWakeupTool implements the ScheduleWakeup tool: starting a
// new self-paced loop (empty task_id) or rescheduling/re-prompting an
// existing one (non-empty task_id) from within its own firing.
func (c *coordinator) scheduleWakeupTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		ScheduleWakeupToolName,
		scheduleWakeupDescription,
		func(ctx context.Context, params ScheduleWakeupParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			if params.DelaySeconds < scheduleMinIntervalSeconds {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("delay_seconds must be at least %d", scheduleMinIntervalSeconds)), nil
			}

			if params.TaskID != "" {
				status, ok := c.schedules.get(params.TaskID)
				if !ok || status.Kind != ScheduleKindWakeup {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown wakeup task %q", params.TaskID)), nil
				}
				if status.State != ScheduleActive {
					return fantasy.NewTextResponse(fmt.Sprintf("%s already stopped (%s); omit task_id to start a new loop.", params.TaskID, status.StopReason)), nil
				}
				c.schedules.clearMissedReschedule(params.TaskID)
				c.schedules.requestReschedule(params.TaskID, params.DelaySeconds, params.Prompt)
				return fantasy.NewTextResponse(fmt.Sprintf("Rescheduled %s to fire again in %ds.", params.TaskID, params.DelaySeconds)), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			if n := c.schedules.countActive(sessionID); n >= scheduleMaxTasksPerSession {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("too many active scheduled tasks (%d); cancel one with ScheduleCancel first", n)), nil
			}

			p, err := c.permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        c.cfg.WorkingDir(),
					ToolCallID:  call.ID,
					ToolName:    ScheduleWakeupToolName,
					Action:      "schedule",
					Description: fmt.Sprintf("Start a self-paced wakeup loop (first check in %ds): %s", params.DelaySeconds, params.Prompt),
					Params:      params,
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return tools.NewPermissionDeniedResponse(), nil
			}

			taskID := newScheduleID(ScheduleKindWakeup)
			now := time.Now()
			status := ScheduledTaskStatus{
				ID:              taskID,
				OriginSessionID: sessionID,
				Kind:            ScheduleKindWakeup,
				Prompt:          params.Prompt,
				IntervalSeconds: params.DelaySeconds,
				NextFireAt:      now.Add(time.Duration(params.DelaySeconds) * time.Second),
				CreatedAt:       now,
				ExpiresAt:       now.Add(scheduleDefaultExpiry),
				State:           ScheduleActive,
			}
			bgCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			c.schedules.register(status, cancel)
			go c.runScheduledTask(bgCtx, taskID)

			return fantasy.NewTextResponse(fmt.Sprintf(
				"Started wakeup loop %s: first check in %ds. It runs in the background; call ScheduleWakeup(task_id=%q, ...) again from within that firing to keep it going, or ScheduleCancel(task_id=%q) to stop it.",
				taskID, params.DelaySeconds, taskID, taskID,
			)), nil
		},
	)
}

// scheduleListTool implements the ScheduleList tool: lists every
// scheduled task (cron or wakeup) that originated from the current
// session.
func (c *coordinator) scheduleListTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		ScheduleListToolName,
		scheduleListDescription,
		func(ctx context.Context, _ ScheduleListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			list := c.schedules.list(sessionID)
			if len(list) == 0 {
				return fantasy.NewTextResponse("No scheduled tasks."), nil
			}
			var b strings.Builder
			for _, s := range list {
				fmt.Fprintf(&b, "- %s [%s/%s] %q", s.ID, s.Kind, s.State, truncateForList(s.Prompt, 60))
				if s.State == ScheduleActive {
					fmt.Fprintf(&b, " -- next fire in %s (run %d", time.Until(s.NextFireAt).Round(time.Second), s.RunCount)
					if s.MaxRuns > 0 {
						fmt.Fprintf(&b, "/%d", s.MaxRuns)
					}
					b.WriteString(")")
				} else {
					fmt.Fprintf(&b, " -- stopped: %s (ran %d times)", s.StopReason, s.RunCount)
				}
				b.WriteString("\n")
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)
}

// scheduleCancelTool implements the ScheduleCancel tool: stops a
// scheduled task (either kind) by ID.
func (c *coordinator) scheduleCancelTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		ScheduleCancelToolName,
		scheduleCancelDescription,
		func(ctx context.Context, params ScheduleCancelParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.TaskID == "" {
				return fantasy.NewTextErrorResponse("task_id is required"), nil
			}
			status, ok := c.schedules.get(params.TaskID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown task %q", params.TaskID)), nil
			}
			if status.State != ScheduleActive {
				return fantasy.NewTextResponse(fmt.Sprintf("%s is already stopped (%s).", params.TaskID, status.StopReason)), nil
			}
			c.schedules.stop(params.TaskID, "canceled")
			return fantasy.NewTextResponse(fmt.Sprintf("Stopped %s.", params.TaskID)), nil
		},
	)
}

// runScheduledTask drives one scheduled task's lifecycle: waiting for
// its next fire time, dispatching a firing, and deciding what happens
// next based on its kind. Runs until the task stops (canceled, expired,
// max_runs reached, or -- for wakeup tasks -- two consecutive missed
// reschedules) or ctx is canceled. Every exit path leaves the task in
// the registry briefly (scheduleLingerAfterStop) so ScheduleList and
// the UI picker can show its terminal state before it disappears.
func (c *coordinator) runScheduledTask(ctx context.Context, taskID string) {
	defer func() {
		go func() {
			time.Sleep(scheduleLingerAfterStop)
			c.schedules.remove(taskID)
		}()
	}()
	for {
		status, ok := c.schedules.get(taskID)
		if !ok || status.State != ScheduleActive {
			return
		}
		if !status.ExpiresAt.IsZero() && time.Now().After(status.ExpiresAt) {
			c.schedules.stop(taskID, "expired")
			return
		}

		wait := time.Until(status.NextFireAt)
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		status, ok = c.schedules.get(taskID)
		if !ok || status.State != ScheduleActive {
			return
		}

		c.fireScheduledTask(ctx, taskID, status)

		status, ok = c.schedules.get(taskID)
		if !ok || status.State != ScheduleActive {
			return
		}
		if status.MaxRuns > 0 && status.RunCount >= status.MaxRuns {
			c.schedules.stop(taskID, "reached max_runs")
			return
		}
	}
}

// fireScheduledTask runs one firing of a scheduled task: it queues the
// task's prompt into the origin session via Coordinator.Run, which
// enqueues behind a busy session exactly like a typed follow-up and
// never interrupts an active turn, then decides the next fire time.
//
// Cron tasks always re-arm on their fixed interval, independent of
// what the fired turn does; fireScheduledTask waits for the firing to
// finish first so slow firings don't overlap. Wakeup tasks wait for
// the fired turn to call ScheduleWakeup again with a new delay; if it
// doesn't within scheduleWakeupFallback, one fallback firing is given
// as a reminder, and if that also goes unanswered the task stops --
// mirroring Claude Code's /loop safety net.
func (c *coordinator) fireScheduledTask(ctx context.Context, taskID string, status ScheduledTaskStatus) {
	prompt := formatFiringPrompt(taskID, status)
	runCtx := context.WithoutCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		resp, err := c.Run(runCtx, status.OriginSessionID, prompt)
		switch {
		case err != nil:
			slog.Warn("Scheduled task run failed", "task_id", taskID, "error", err)
			c.schedules.recordRun(taskID, "error: "+err.Error())
		case resp == nil:
			// Session was busy; the prompt was queued and will run as
			// a later turn once the session is idle.
			c.schedules.recordRun(taskID, "queued")
		default:
			c.schedules.recordRun(taskID, "ran")
		}
	}()

	if status.Kind == ScheduleKindCron {
		<-done
		c.schedules.setNextFire(taskID, time.Now().Add(time.Duration(status.IntervalSeconds)*time.Second))
		return
	}

	c.schedules.awaitWakeupReschedule(ctx, taskID, scheduleWakeupFallback)
}

// formatFiringPrompt builds the prompt actually sent into the origin
// session for a firing, wrapping the task's own prompt with enough
// context (task ID, kind, how to keep it going or stop it) that the
// model can act on it without needing to call ScheduleList first.
func formatFiringPrompt(taskID string, status ScheduledTaskStatus) string {
	switch status.Kind {
	case ScheduleKindCron:
		return fmt.Sprintf(
			"[Scheduled task %s fired (run %d%s)]\n\n%s\n\nThis is an automated recurring check; it will fire again in %ds on its own. Call ScheduleCancel(task_id=%q) if it's no longer needed.",
			taskID, status.RunCount+1, maxRunsSuffix(status.MaxRuns), status.Prompt, status.IntervalSeconds, taskID,
		)
	default: // ScheduleKindWakeup
		return fmt.Sprintf(
			"[Scheduled wakeup %s fired]\n\n%s\n\nTo keep checking, call ScheduleWakeup(task_id=%q, prompt=..., delay_seconds=...) with a new delay before you finish this turn. If you don't, this loop gets one more reminder wakeup and then stops on its own. Call ScheduleCancel(task_id=%q) to end it now instead.",
			taskID, status.Prompt, taskID, taskID,
		)
	}
}

func maxRunsSuffix(maxRuns int) string {
	if maxRuns <= 0 {
		return ""
	}
	return fmt.Sprintf(", up to %d times", maxRuns)
}

func newScheduleID(kind ScheduleKind) string {
	prefix := "cron"
	if kind == ScheduleKindWakeup {
		prefix = "wake"
	}
	return prefix + "-" + uuid.New().String()[:8]
}

func truncateForList(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// RunningSchedules implements Coordinator.
func (c *coordinator) RunningSchedules() []ScheduledTaskStatus {
	return c.schedules.listAll()
}

// CancelSchedule implements Coordinator.
func (c *coordinator) CancelSchedule(taskID string) {
	c.schedules.stop(taskID, "canceled")
}
