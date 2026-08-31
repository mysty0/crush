package agent

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
)

// pollGuardThreshold is how many consecutive blocking-wait tool calls (see
// pollClassBlockingWait) a session may make, with no other work in between,
// before the guard starts refusing them. 1 means: the first wait is always
// allowed, the very next consecutive one is not.
const pollGuardThreshold = 1

// sleepOnlyCommand matches a Bash command whose entire body is a sleep
// statement, optionally followed by a trivial echo (a common "so I can see
// it's done" pattern) -- i.e. a call whose only purpose is to block the
// turn for a while rather than to do any real work. Only relevant when the
// call runs in the foreground (run_in_background=false): a backgrounded
// sleep returns immediately and does not itself block the turn, so it is
// real, useful work (arming a timer to check later) rather than a wait.
var sleepOnlyCommand = regexp.MustCompile(`(?s)^\s*sleep\s+[0-9]+(\.[0-9]+)?\s*([;&]{1,2}\s*echo\b[^\n]*)?\s*;?\s*$`)

type pollClass int

const (
	// pollClassWork is any tool call that represents real work. It
	// resets a session's blocking-wait streak to zero.
	pollClassWork pollClass = iota
	// pollClassCheck is a free, instantly-returning progress check
	// (AgentProgress, AgentList, or job_output without wait). It is
	// always allowed and leaves the streak untouched -- checking often
	// is fine, it is the *blocking wait* that is expensive.
	pollClassCheck
	// pollClassBlockingWait is a call that blocks the turn for a
	// meaningful stretch of real time waiting on a background task (a
	// bare `sleep`, or job_output with wait=true). It grows the streak
	// and is refused once the streak exceeds pollGuardThreshold.
	pollClassBlockingWait
)

// pollGuardDenialMessage explains to the model why a blocking-wait call was
// refused and what to do instead. It intentionally does not just say "stop
// polling" -- it names the two concrete replacements (trusting background
// delivery, or ScheduleWakeup) so the model has an actionable next step
// rather than being left to guess.
const pollGuardDenialMessage = `This call was not executed: you've made two consecutive blocking waits (a bare "sleep" command, or job_output with wait=true) for a background task without doing any other work or ending your turn in between.

Each wait long enough to matter risks expiring the provider's prompt cache, which re-sends and re-caches the entire conversation on the next request -- that is real wasted cost, not just wall-clock time.

If this is waiting on something started with agent(background: true) or the Workflow tool, its result is delivered here automatically as a follow-up message the moment it finishes -- you do not need to poll for it at all. If you genuinely need to check back later, use ScheduleWakeup(delay_seconds=N) instead: it defers the check without blocking this turn. Otherwise, do other useful work now, or end your turn and wait for the result to arrive.`

// pollGuard tracks, per session, a streak of consecutive blocking-wait tool
// calls and refuses to run one past pollGuardThreshold. See
// pollGuardDenialMessage for the rationale surfaced to the model.
type pollGuard struct {
	mu     sync.Mutex
	streak map[string]int
}

func newPollGuard() *pollGuard {
	return &pollGuard{streak: make(map[string]int)}
}

// observe classifies one tool call's effect on a session's streak and
// reports whether it should be allowed to run.
func (g *pollGuard) observe(sessionID string, class pollClass) (allowed bool) {
	// A nil guard shows up in tests that build a bare &coordinator{}
	// without going through NewCoordinator; treat it as "no guard
	// configured" rather than crashing.
	if g == nil || sessionID == "" {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch class {
	case pollClassCheck:
		return true
	case pollClassBlockingWait:
		g.streak[sessionID]++
		return g.streak[sessionID] <= pollGuardThreshold
	default: // pollClassWork
		g.streak[sessionID] = 0
		return true
	}
}

// classifyPollCall inspects a tool call's name and raw input to decide its
// pollClass. Unrecognized tools, and any call whose input fails to parse,
// default to pollClassWork -- the guard only ever restricts the specific
// calls it can positively identify as a blocking wait.
func classifyPollCall(toolName, input string) pollClass {
	switch toolName {
	case tools.BashToolName:
		var p struct {
			Command         string `json:"command"`
			RunInBackground bool   `json:"run_in_background"`
		}
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			return pollClassWork
		}
		if sleepOnlyCommand.MatchString(p.Command) {
			if p.RunInBackground {
				// Arms a background timer and returns immediately --
				// doesn't block, but isn't real progress either. Must
				// not reset the streak, or re-arming a new sleep before
				// every wait (exactly what caused the incident this
				// guard exists for) would erase it right before the
				// call that actually matters.
				return pollClassCheck
			}
			return pollClassBlockingWait
		}
		return pollClassWork
	case tools.JobOutputToolName:
		var p struct {
			Wait bool `json:"wait"`
		}
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			return pollClassWork
		}
		if p.Wait {
			return pollClassBlockingWait
		}
		return pollClassCheck
	// AgentListToolName / AgentProgressToolName (agent_status_tool.go):
	// literals used here rather than the constants to avoid depending on
	// this file's exact neighbors; both name this session's own status
	// tools and never block.
	case "AgentList", "AgentProgress":
		return pollClassCheck
	default:
		return pollClassWork
	}
}

// pollGuardedTool wraps a fantasy.AgentTool so a call classified as
// pollClassBlockingWait is refused once the owning session's streak is
// already over threshold.
type pollGuardedTool struct {
	inner fantasy.AgentTool
	guard *pollGuard
}

func (p *pollGuardedTool) Info() fantasy.ToolInfo {
	return p.inner.Info()
}

func (p *pollGuardedTool) ProviderOptions() fantasy.ProviderOptions {
	return p.inner.ProviderOptions()
}

func (p *pollGuardedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	p.inner.SetProviderOptions(opts)
}

func (p *pollGuardedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := tools.GetSessionFromContext(ctx)
	class := classifyPollCall(p.inner.Info().Name, call.Input)
	if !p.guard.observe(sessionID, class) {
		return fantasy.NewTextErrorResponse(pollGuardDenialMessage), nil
	}
	return p.inner.Run(ctx, call)
}

// wrapToolsWithPollGuard decorates every tool with the poll-loop guard.
// Every tool participates, not just Bash/job_output/AgentProgress/AgentList,
// because a real tool call (an edit, a read, a grep) must reset the streak
// -- only the guard observing literally every call in order can tell a
// genuine "wait, do other work, wait again" sequence apart from a bare
// poll loop.
func wrapToolsWithPollGuard(ts []fantasy.AgentTool, guard *pollGuard) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(ts))
	for i, tool := range ts {
		out[i] = &pollGuardedTool{inner: tool, guard: guard}
	}
	return out
}
