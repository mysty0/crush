package agent

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// awaitTaskStatus reads events off sub until one matching want arrives,
// failing the test if none does within a short timeout. The broker is
// package-level and shared across parallel tests, so events from other
// tests may interleave on the same channel; filtering by Ref keeps each
// test deterministic without sleeps.
func awaitTaskStatus(t *testing.T, sub <-chan pubsub.Event[TaskStatusEvent], want TaskRef) TaskStatusEvent {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-sub:
			require.Equal(t, pubsub.UpdatedEvent, ev.Type)
			if ev.Payload.Ref == want {
				return ev.Payload
			}
		case <-deadline:
			t.Fatalf("timed out waiting for task status event with ref %+v", want)
			return TaskStatusEvent{}
		}
	}
}

// TestSubAgentRegistry_PublishesTaskStatus verifies register and finish
// each publish an observable event on the unified task-status stream.
func TestSubAgentRegistry_PublishesTaskStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := SubscribeTaskStatus(ctx)

	r := newSubAgentRegistry()
	ref := TaskRef{Kind: TaskKindSubAgent, ID: "sub-1"}
	r.register(SubAgentStatus{
		SessionID:       "sub-1",
		ParentSessionID: "parent-1",
		ToolName:        "agent",
		State:           SubAgentRunning,
		StartedAt:       time.Now(),
	})
	got := awaitTaskStatus(t, sub, ref)
	require.Equal(t, "parent-1", got.ParentSessionID)

	r.finish("sub-1", SubAgentDone, "")
	got = awaitTaskStatus(t, sub, ref)
	require.Equal(t, "parent-1", got.ParentSessionID)
}

// TestWorkflowRegistry_PublishesTaskStatus verifies register and finish
// each publish an observable event on the unified task-status stream.
func TestWorkflowRegistry_PublishesTaskStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := SubscribeTaskStatus(ctx)

	r := newWorkflowRegistry()
	ref := TaskRef{Kind: TaskKindWorkflow, ID: "wf-1"}
	r.register(WorkflowStatus{
		SessionID:       "wf-1",
		ParentSessionID: "parent-2",
		Name:            "deep-research",
		State:           WorkflowRunning,
		StartedAt:       time.Now(),
	}, nil)
	got := awaitTaskStatus(t, sub, ref)
	require.Equal(t, "parent-2", got.ParentSessionID)

	r.finish("wf-1", WorkflowCompleted, "done", "")
	got = awaitTaskStatus(t, sub, ref)
	require.Equal(t, "parent-2", got.ParentSessionID)
}

// TestScheduleRegistry_PublishesTaskStatus verifies register and stop
// each publish an observable event on the unified task-status stream.
func TestScheduleRegistry_PublishesTaskStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := SubscribeTaskStatus(ctx)

	r := newScheduleRegistry()
	ref := TaskRef{Kind: TaskKindSchedule, ID: "sched-1"}
	r.register(ScheduledTaskStatus{
		ID:              "sched-1",
		OriginSessionID: "parent-3",
		Kind:            ScheduleKindCron,
		State:           ScheduleActive,
		CreatedAt:       time.Now(),
	}, nil)
	got := awaitTaskStatus(t, sub, ref)
	require.Equal(t, "parent-3", got.ParentSessionID)

	r.stop("sched-1", "canceled")
	got = awaitTaskStatus(t, sub, ref)
	require.Equal(t, "parent-3", got.ParentSessionID)
}
