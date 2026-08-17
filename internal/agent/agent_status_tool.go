package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
)

const (
	// AgentListToolName lists the background tasks dispatched from the
	// current session.
	AgentListToolName = "AgentList"
	// AgentProgressToolName reports detailed live progress for one
	// background task.
	AgentProgressToolName = "AgentProgress"
)

//go:embed templates/agent_list.md
var agentListDescription string

//go:embed templates/agent_progress.md
var agentProgressDescription string

// AgentListParams are the parameters for the AgentList tool (none).
type AgentListParams struct{}

// AgentProgressParams are the parameters for the AgentProgress tool.
type AgentProgressParams struct {
	SessionID string `json:"session_id" description:"The background task's ID, as reported by AgentList or by the tool call that started it: a sub-agent or workflow session ID, or a scheduled task ID."`
}

// taskDispatcher names the tool that dispatches a given kind of
// background task, so a listed row tells the model which tool the task
// came from rather than exposing the internal kind discriminator.
func taskDispatcher(kind TaskKind) string {
	switch kind {
	case TaskKindAgenticFetch:
		return "agentic_fetch"
	case TaskKindWorkflow:
		return WorkflowToolName
	case TaskKindSchedule:
		return "Schedule"
	case TaskKindBash:
		return "Bash"
	default:
		return AgentToolName
	}
}

// agentListTool implements the AgentList tool: lists every background
// task owned by the current session -- sub-agents, agentic fetches,
// workflows, and scheduled tasks -- running or recently finished.
//
// It reads the unified task registry (Tasks) rather than any single
// per-kind registry, so every ID this session was handed by a
// dispatching tool resolves here, whichever registry actually owns it.
func (c *coordinator) agentListTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		AgentListToolName,
		agentListDescription,
		func(ctx context.Context, _ AgentListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			tasks := c.Tasks(sessionID)
			if len(tasks) == 0 {
				return fantasy.NewTextResponse("No background tasks (sub-agents, workflows, or scheduled tasks) dispatched from this session."), nil
			}
			var b strings.Builder
			for _, t := range tasks {
				fmt.Fprintf(&b, "- %s [%s/%s] %q", t.Ref.ID, taskDispatcher(t.Ref.Kind), t.State, truncateForList(t.Label, 60))
				switch {
				case t.State == TaskRunning:
					fmt.Fprintf(&b, " -- running for %s", time.Since(t.StartedAt).Round(time.Second))
				case !t.FinishedAt.IsZero():
					fmt.Fprintf(&b, " -- %s after %s", t.State, t.FinishedAt.Sub(t.StartedAt).Round(time.Second))
				}
				if sa, ok := t.Detail.(SubAgentStatus); ok && sa.Error != "" {
					fmt.Fprintf(&b, ": %s", truncateForList(sa.Error, 80))
				}
				b.WriteString("\n")
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)
}

// agentProgressTool implements the AgentProgress tool: detailed live
// progress for one background task, resolved by ID through the unified
// registry and then rendered per kind, since what "progress" means
// differs (a sub-agent's message history, a workflow's phase pipeline,
// a schedule's firing count).
func (c *coordinator) agentProgressTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		AgentProgressToolName,
		agentProgressDescription,
		func(ctx context.Context, params AgentProgressParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.SessionID == "" {
				return fantasy.NewTextErrorResponse("session_id is required"), nil
			}
			task, ok := c.taskByID(params.SessionID)
			if !ok {
				// Not in any live registry. A finished sub-agent is
				// reaped shortly after it ends, so a poller asking a
				// moment too late must not be told the task never
				// existed -- its session is still on disk with the
				// full transcript. Answer from there instead.
				return fantasy.NewTextResponse(c.finishedTaskReport(ctx, params.SessionID)), nil
			}
			switch detail := task.Detail.(type) {
			case WorkflowStatus:
				return fantasy.NewTextResponse(workflowProgressReport(detail)), nil
			case ScheduledTaskStatus:
				return fantasy.NewTextResponse(scheduleProgressReport(detail)), nil
			default:
				sub, _ := task.Detail.(SubAgentStatus)
				return fantasy.NewTextResponse(c.subAgentProgressReport(ctx, sub)), nil
			}
		},
	)
}

// finishedTaskReport answers AgentProgress for a task that is no longer
// in any live registry, by reading its persisted session.
//
// A finished sub-agent lingers in the registry only briefly (see
// subAgentLingerAfterFinish) before being reaped, while a caller
// polling on a timer may well ask minutes later. Reporting "unknown"
// then is both wrong and expensive: it reads as "this task never ran",
// which invites re-dispatching work that already completed and whose
// result has usually already been delivered. The session outlives the
// registry entry, so it is the authoritative source once the entry is
// gone.
func (c *coordinator) finishedTaskReport(ctx context.Context, sessionID string) string {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return unknownTaskReport(sessionID)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Session: %s\nTask: %s\nState: finished (no longer tracked as running)\n",
		sess.ID, sess.Title)
	if sess.ParentSessionID != "" {
		fmt.Fprintf(&b, "Dispatched from: %s\n", sess.ParentSessionID)
	}
	fmt.Fprintf(&b, "Messages in its transcript: %d\n", sess.MessageCount)

	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return b.String() + "\n(failed to load message history: " + err.Error() + ")"
	}

	var (
		toolCalls int
		lastText  string
		outcome   string
	)
	for i := range msgs {
		msg := &msgs[i]
		toolCalls += len(msg.ToolCalls())
		if text := strings.TrimSpace(msg.Content().Text); text != "" {
			lastText = text
		}
		if msg.Role == message.Assistant {
			switch msg.FinishReason() {
			case message.FinishReasonEndTurn:
				outcome = "completed normally"
			case message.FinishReasonCanceled:
				outcome = "was canceled"
			case message.FinishReasonError:
				outcome = "ended with an error"
			case message.FinishReasonMaxTokens:
				outcome = "hit its output limit"
			}
		}
	}
	if outcome != "" {
		fmt.Fprintf(&b, "Outcome: it %s.\n", outcome)
	}
	fmt.Fprintf(&b, "Tool calls made: %d\n", toolCalls)
	if lastText != "" {
		fmt.Fprintf(&b, "Final output:\n%s\n", truncateForList(lastText, 2000))
	}
	b.WriteString(
		"\nThis task is done, so it is no longer listed by AgentList. " +
			"If it ran in the background its result has already been, or " +
			"shortly will be, delivered into the dispatching conversation " +
			"as a follow-up message -- check there before re-dispatching it.\n",
	)
	return b.String()
}

// unknownTaskReport is the genuinely-not-found response: no live
// registry entry and no stored session.
//
// It spells out the ID format because a sub-agent session ID is
// "<messageID>$$<toolCallID>" (see session.CreateAgentToolSessionID),
// and trimming the "$$..." suffix leaves a message ID that matches no
// session at all -- which looks like independent proof the task never
// existed.
func unknownTaskReport(sessionID string) string {
	return fmt.Sprintf(
		"No background task or stored session matches %q.\n\n"+
			"This means the ID itself is unrecognized, not that a task failed. "+
			"Check it is complete: a sub-agent session ID is the full "+
			"\"<messageID>$$<toolCallID>\" string, including the \"$$\" and "+
			"everything after it. Use AgentList to see currently-running tasks.",
		sessionID,
	)
}

// subAgentProgressReport renders one sub-agent's progress, computed
// from its persisted session message history the same way the workflow
// view's per-agent stats are.
func (c *coordinator) subAgentProgressReport(ctx context.Context, status SubAgentStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session: %s\nDispatched by: %s\nTask: %s\nModel: %s/%s\nState: %s\n",
		status.SessionID, status.ToolName, status.Label, status.Provider, status.Model, status.State)
	switch status.State {
	case SubAgentRunning:
		fmt.Fprintf(&b, "Running for: %s\n", time.Since(status.StartedAt).Round(time.Second))
	default:
		fmt.Fprintf(&b, "Duration: %s\n", status.FinishedAt.Sub(status.StartedAt).Round(time.Second))
		if status.Error != "" {
			fmt.Fprintf(&b, "Error: %s\n", status.Error)
		}
	}

	msgs, err := c.messages.List(ctx, status.SessionID)
	if err != nil {
		return b.String() + "\n(failed to load message history: " + err.Error() + ")"
	}

	var (
		toolCalls    int
		lastToolCall string
		lastText     string
	)
	for i := range msgs {
		msg := &msgs[i]
		for _, tc := range msg.ToolCalls() {
			toolCalls++
			if !tc.Finished {
				lastToolCall = tc.Name + " (in progress)"
			} else {
				lastToolCall = tc.Name
			}
		}
		if text := strings.TrimSpace(msg.Content().Text); text != "" {
			lastText = text
		}
	}
	fmt.Fprintf(&b, "Tool calls so far: %d\n", toolCalls)
	if lastToolCall != "" {
		fmt.Fprintf(&b, "Most recent tool call: %s\n", lastToolCall)
	}
	if lastText != "" {
		fmt.Fprintf(&b, "Latest output:\n%s\n", truncateForList(lastText, 500))
	}
	return b.String()
}

// workflowProgressReport renders a running workflow's progress from the
// workflow registry: its phase pipeline and the sub-agents each phase
// dispatched. A workflow session runs no turn of its own -- all of its
// work happens in child sessions -- so its own message history is empty
// and cannot be used the way a sub-agent's is.
func workflowProgressReport(w WorkflowStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session: %s\nDispatched by: %s\nWorkflow: %s\n", w.SessionID, WorkflowToolName, w.Name)
	if w.Args != "" {
		fmt.Fprintf(&b, "Args: %s\n", w.Args)
	}
	fmt.Fprintf(&b, "State: %s\n", w.State)
	if w.State == WorkflowRunning {
		fmt.Fprintf(&b, "Running for: %s\n", time.Since(w.StartedAt).Round(time.Second))
	}

	if len(w.Phases) > 0 {
		b.WriteString("Phases (* is the current one):\n")
		for _, p := range w.Phases {
			marker := " "
			if p.Active {
				marker = "*"
			}
			fmt.Fprintf(&b, "  %s %s -- %d agents\n", marker, p.Name, p.AgentCount)
		}
	}

	if len(w.Agents) > 0 {
		var running int
		for _, a := range w.Agents {
			if !a.Done {
				running++
			}
		}
		fmt.Fprintf(&b, "Agents: %d dispatched, %d still running\n", len(w.Agents), running)
		for _, a := range w.Agents {
			state := "done"
			if !a.Done {
				state = "running for " + time.Since(a.StartedAt).Round(time.Second).String()
			}
			fmt.Fprintf(&b, "  - %s [%s] %q on %s -- %s\n",
				a.SessionID, a.Phase, truncateForList(a.Label, 60), a.Model, state)
		}
	}

	if w.Summary != "" {
		fmt.Fprintf(&b, "Summary: %s\n", w.Summary)
	}
	if w.ReportPath != "" {
		fmt.Fprintf(&b, "Report: %s\n", w.ReportPath)
	}
	return b.String()
}

// scheduleProgressReport renders a scheduled task's progress: how often
// it has fired, when it fires next, and why it stopped if it has.
func scheduleProgressReport(s ScheduledTaskStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task: %s\nKind: %s\nPrompt: %s\nState: %s\n",
		s.ID, s.Kind, truncateForList(s.Prompt, 200), s.State)
	fmt.Fprintf(&b, "Firings so far: %d", s.RunCount)
	if s.MaxRuns > 0 {
		fmt.Fprintf(&b, " of %d", s.MaxRuns)
	}
	b.WriteString("\n")
	if s.State == ScheduleActive && !s.NextFireAt.IsZero() {
		fmt.Fprintf(&b, "Next firing: in %s\n", time.Until(s.NextFireAt).Round(time.Second))
	}
	if s.LastResult != "" {
		fmt.Fprintf(&b, "Most recent firing: %s\n", s.LastResult)
	}
	if s.StopReason != "" {
		fmt.Fprintf(&b, "Stopped because: %s\n", s.StopReason)
	}
	return b.String()
}
