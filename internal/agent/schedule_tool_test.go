package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMaxRunsSuffix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", maxRunsSuffix(0))
	assert.Equal(t, "", maxRunsSuffix(-1))
	assert.Equal(t, ", up to 5 times", maxRunsSuffix(5))
}

func TestNewScheduleID(t *testing.T) {
	t.Parallel()
	cronID := newScheduleID(ScheduleKindCron)
	assert.True(t, strings.HasPrefix(cronID, "cron-"))

	wakeID := newScheduleID(ScheduleKindWakeup)
	assert.True(t, strings.HasPrefix(wakeID, "wake-"))

	// IDs should be unique across calls.
	assert.NotEqual(t, newScheduleID(ScheduleKindCron), newScheduleID(ScheduleKindCron))
}

func TestTruncateForList(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "short", truncateForList("short", 60))
	assert.Equal(t, "  trimmed  "[2:9], truncateForList("  trimmed  ", 60))

	long := strings.Repeat("a", 100)
	got := truncateForList(long, 10)
	assert.Equal(t, strings.Repeat("a", 10)+"...", got)
	assert.Len(t, got, 13)
}

func TestFormatFiringPrompt_Cron(t *testing.T) {
	t.Parallel()
	status := ScheduledTaskStatus{
		Kind:            ScheduleKindCron,
		Prompt:          "check the build",
		IntervalSeconds: 300,
		RunCount:        2,
		MaxRuns:         5,
	}
	prompt := formatFiringPrompt("cron-abc123", status)

	assert.Contains(t, prompt, "cron-abc123")
	assert.Contains(t, prompt, "check the build")
	assert.Contains(t, prompt, "run 3")
	assert.Contains(t, prompt, "up to 5 times")
	assert.Contains(t, prompt, "will fire again in 300s")
	assert.Contains(t, prompt, "ScheduleCancel(task_id=\"cron-abc123\")")
}

func TestFormatFiringPrompt_CronUnbounded(t *testing.T) {
	t.Parallel()
	status := ScheduledTaskStatus{
		Kind:            ScheduleKindCron,
		Prompt:          "check the build",
		IntervalSeconds: 300,
	}
	prompt := formatFiringPrompt("cron-abc123", status)
	assert.NotContains(t, prompt, "up to")
}

func TestFormatFiringPrompt_Wakeup(t *testing.T) {
	t.Parallel()
	status := ScheduledTaskStatus{
		Kind:   ScheduleKindWakeup,
		Prompt: "check the deploy",
	}
	prompt := formatFiringPrompt("wake-xyz789", status)

	assert.Contains(t, prompt, "wake-xyz789")
	assert.Contains(t, prompt, "check the deploy")
	assert.Contains(t, prompt, "ScheduleWakeup(task_id=\"wake-xyz789\"")
	assert.Contains(t, prompt, "ScheduleCancel(task_id=\"wake-xyz789\")")
	assert.Contains(t, prompt, "one more reminder wakeup")
}

// Sanity check that the scheduler's minimum-interval and expiry
// constants are sane relative to each other (a wakeup fallback that
// exceeds the task expiry, or a minimum interval below zero, would be
// a silent misconfiguration).
func TestScheduleConstants_Sane(t *testing.T) {
	t.Parallel()
	assert.Greater(t, scheduleMinIntervalSeconds, 0)
	assert.Greater(t, scheduleMaxTasksPerSession, 0)
	assert.Less(t, scheduleWakeupFallback, scheduleDefaultExpiry)
	assert.Positive(t, time.Duration(scheduleMinIntervalSeconds)*time.Second)
}
