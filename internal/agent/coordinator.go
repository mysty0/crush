package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/compressd"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/discover"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/hashline"
	"github.com/charmbracelet/crush/internal/hashline/tsblock"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/charmbracelet/crush/internal/oauth/codex"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/oauth/geminicli"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"golang.org/x/sync/errgroup"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/qjebbs/go-jsons"
)

// Coordinator errors.
var (
	errCoderAgentNotConfigured    = errors.New("coder agent not configured")
	errModelProviderNotConfigured = errors.New("model provider not configured")
	errLargeModelNotSelected      = errors.New("large model not selected")
	errSmallModelNotSelected      = errors.New("small model not selected")
	errTaskAgentNotConfigured     = errors.New("task agent not configured")
	errSubAgentNotRunning         = errors.New("sub-agent is not currently running")
	errModelNotFound              = errors.New("model not found in provider config")
)

// Copilot models that use the Responses API instead of Chat Completions.
var copilotResponsesModels = map[string]bool{
	"gpt-5.2":       true,
	"gpt-5.2-codex": true,
	"gpt-5.3-codex": true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.5":       true,
	"gpt-5-mini":    true,
}

// OpenCode models that use the Anthropic Messages API instead of Chat Completions.
var opencodeMessagesModels = map[string]bool{
	"qwen3.7-max": true,
}

// defaultLegacyThinkBudgetTokens is the reasoning token budget applied for
// Anthropic/Bedrock models when the legacy boolean Think toggle is on but
// no discrete reasoning level has been chosen yet.
const defaultLegacyThinkBudgetTokens = 2000

// defaultLegacyThinkLevel is the reasoning level applied for Google models
// when the legacy boolean Think toggle is on but no discrete reasoning
// level has been chosen yet.
const defaultLegacyThinkLevel = "low"

type Coordinator interface {
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunAccepted runs a call that was already accepted via
	// BeginAccepted on the fire-and-forget dispatch path. The handle is
	// the only carrier of accept-state across the backend.runAgent /
	// Coordinator / sessionAgent.Run layers: it reaches
	// sessionAgent.Run as SessionAgentCall.Accepted, where it is
	// consumed under dispatchMu once the accepted -> (cancel-on-entry |
	// queued | active) transition is chosen.
	RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *AcceptedRun
	Cancel(sessionID string)
	// CancelKeepQueue stops the active run without discarding queued
	// follow-up prompts, so the first queued prompt starts as the next
	// turn once the canceled run unwinds.
	CancelKeepQueue(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	Summarize(context.Context, string) error
	Model() Model
	UpdateModels(ctx context.Context) error
	// GenerateTitle generates a session title using the current agent. It
	// returns errCoderAgentNotConfigured if no agent is configured.
	GenerateTitle(ctx context.Context, sessionID, prompt string) error
	// RegenerateTitle re-runs AI title generation for a session using its
	// first user message. It returns an error if the session has no user
	// message to title from.
	RegenerateTitle(ctx context.Context, sessionID string) error
	// SendToSubAgent steers a currently-running sub-agent turn
	// (dispatched via the "agent" tool) by injecting a follow-up
	// user prompt into its session. It returns an error if the task
	// agent is not configured or the sub-agent session is not
	// currently busy (e.g. it already finished): follow-ups are only
	// supported while the sub-agent turn is in progress.
	SendToSubAgent(ctx context.Context, subAgentSessionID, prompt string) error
	// CancelSubAgent cancels a currently-running sub-agent turn
	// (dispatched via the "agent" tool). It is a no-op if the
	// sub-agent session is not currently busy or the task agent is
	// not configured.
	CancelSubAgent(subAgentSessionID string)
	// RunningWorkflows returns a snapshot of every background workflow
	// (dispatched via the "Workflow" tool) known to the coordinator,
	// running or recently finished but not yet cleared.
	RunningWorkflows() []WorkflowStatus
	// WorkflowStatus returns the current status of the workflow with
	// the given (workflow) session ID, if known.
	WorkflowStatus(workflowSessionID string) (WorkflowStatus, bool)
	// CancelWorkflow cancels a running background workflow by its
	// (workflow) session ID. It is a no-op if the workflow is unknown
	// or already finished.
	CancelWorkflow(workflowSessionID string)
	// RunningSchedules returns a snapshot of every scheduled task
	// (dispatched via ScheduleCron/ScheduleWakeup) known to the
	// coordinator, active or recently stopped.
	RunningSchedules() []ScheduledTaskStatus
	// CancelSchedule stops a scheduled task by its task ID. It is a
	// no-op if the task is unknown or already stopped.
	CancelSchedule(taskID string)
	// Tasks returns the unified list of background tasks (sub-agents,
	// workflows, scheduled tasks) owned by parentSessionID, for the UI's
	// task picker. See TaskStatus.
	Tasks(parentSessionID string) []TaskStatus
	// SubscribeTasks returns a channel that receives status-change
	// events for every kind of background task (sub-agents,
	// workflows, scheduled tasks) across every registry, so a
	// subscriber can watch the unified task list update without
	// polling. The broker is package-level, so every coordinator
	// instance shares the same event stream (no per-instance
	// filtering).
	SubscribeTasks(ctx context.Context) <-chan pubsub.Event[TaskStatusEvent]
	// ReconcileStuckSession scans sessionID and every descendant
	// session (sub-agent and workflow sessions reachable through the
	// session tree) for tool calls left unfinished by a run that
	// never got to persist its terminal state -- e.g. the app was
	// closed or crashed mid-turn. It writes the same synthetic-
	// cancellation records a live run's own error path would have
	// written, so the UI stops rendering them as perpetually running.
	// A session with a genuinely live run (or any live ancestor) is
	// left untouched, along with its descendants. Returns the number
	// of tool calls that were reconciled.
	ReconcileStuckSession(ctx context.Context, sessionID string) (int, error)
	// BackgroundNow signals every currently-blocking operation for
	// sessionID (a foreground bash command's wait, or a synchronous
	// sub-agent/agentic_fetch turn dispatched via runSubAgent) to detach
	// and continue running in the background. Returns how many were
	// fired, broken down by kind, so the Ctrl+B keybinding can report a
	// precise summary. A nil/empty result means nothing was blocking.
	BackgroundNow(sessionID string) map[tools.BackgroundKind]int
}

type coordinator struct {
	cfg         *config.ConfigStore
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	history     history.Service
	filetracker filetracker.Service
	lspManager  *lsp.Manager
	notify      pubsub.Publisher[notify.Notification]
	runComplete pubsub.Publisher[notify.RunComplete]
	memory      *memory.Store
	// compressdMgr supervises the local headroomd compression daemon used
	// to shrink large stored tool-result messages from prior turns before
	// they are resent to the model. Nil when not wired up (e.g. tests).
	compressdMgr *compressd.Manager
	// retrieveStore holds the original content of tool-result messages
	// that compressd replaced with a summary + compressed text, so the
	// retrieve_full_output tool can return it if the model asks.
	retrieveStore *compressd.RetrievalStore

	currentAgent SessionAgent
	agents       map[string]SessionAgent
	// taskAgents holds every task-agent SessionAgent instance the
	// "agent" tool can dispatch sub-agent turns to, keyed by the primary
	// model ID it runs on. The default (configured large) agent is added
	// on every UpdateModels call; additional per-model instances are
	// built lazily the first time the LLM requests that model via the
	// tool's "model" parameter. Instances are pruned once idle, but a
	// busy instance is always kept so an in-flight sub-agent turn stays
	// steerable/cancelable even after the user switches models.
	// SendToSubAgent and CancelSubAgent iterate every instance to find
	// the one that owns a given sub-agent session (a session runs on
	// exactly one instance).
	taskAgents *csync.Map[string, SessionAgent]
	// defaultTaskAgentKey is the registry key of the eagerly-built default
	// (read-only, configured large model) task agent, used so it is never
	// pruned and so the tool can dispatch to it without a rebuild.
	defaultTaskAgentKey *csync.Value[string]

	// workflows tracks background workflow runs (dispatched via the
	// "Workflow" tool) so they can be listed, viewed, and canceled
	// while they run in the background.
	workflows *workflowRegistry

	// schedules tracks background scheduled tasks (dispatched via the
	// ScheduleCron/ScheduleWakeup tools) so they can be listed and
	// canceled while they run.
	schedules *scheduleRegistry

	// subAgents tracks every sub-agent invocation dispatched via the
	// agent, agentic_fetch, and Workflow tools, so AgentList and
	// AgentProgress can report on them while they run.
	subAgents *subAgentRegistry

	// Skills discovery results (session-start snapshot).
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker
	// loadedSkills holds skills activated per session so their
	// instructions are re-injected each turn (persists across turns and
	// summarization).
	loadedSkills *skills.LoadedStore

	// snapshots binds hashline section tags to the exact file content that
	// minted them, per session. Shared between the read tool (producer) and
	// the hashline edit tool (consumer). Only used when EditMode is hashline.
	snapshots *hashline.Store

	readyWg errgroup.Group
}

func NewCoordinator(
	ctx context.Context,
	cfg *config.ConfigStore,
	sessions session.Service,
	messages message.Service,
	permissions permission.Service,
	history history.Service,
	filetracker filetracker.Service,
	lspManager *lsp.Manager,
	notify pubsub.Publisher[notify.Notification],
	runComplete pubsub.Publisher[notify.RunComplete],
	skillsMgr *skills.Manager,
	mem *memory.Store,
	compressdMgr *compressd.Manager,
) (Coordinator, error) {
	// Skills are pre-discovered by the caller (see app.New /
	// backend.CreateWorkspace) and passed in via the manager. If no
	// manager was provided (legacy callers), fall back to an in-line
	// discovery so the coordinator still works.
	var allSkills, activeSkills []*skills.Skill
	if skillsMgr != nil {
		allSkills = skillsMgr.AllSkills()
		activeSkills = skillsMgr.ActiveSkills()
	} else {
		allSkills, activeSkills = discoverSkills(cfg)
	}
	skillTracker := skills.NewTracker(activeSkills)

	c := &coordinator{
		cfg:                 cfg,
		sessions:            sessions,
		messages:            messages,
		permissions:         permissions,
		history:             history,
		filetracker:         filetracker,
		lspManager:          lspManager,
		notify:              notify,
		runComplete:         runComplete,
		agents:              make(map[string]SessionAgent),
		taskAgents:          csync.NewMap[string, SessionAgent](),
		defaultTaskAgentKey: csync.NewValue[string](""),
		workflows:           newWorkflowRegistry(),
		schedules:           newScheduleRegistry(),
		subAgents:           newSubAgentRegistry(),
		allSkills:           allSkills,
		activeSkills:        activeSkills,
		skillTracker:        skillTracker,
		loadedSkills:        skills.NewLoadedStore(),
		snapshots:           hashline.NewStore(),
		memory:              mem,
		compressdMgr:        compressdMgr,
		retrieveStore:       compressd.NewRetrievalStore(),
	}

	agentCfg, ok := cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	// TODO: make this dynamic when we support multiple agents
	prompt, err := coderPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, prompt, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.currentAgent = agent
	c.agents[config.AgentCoder] = agent
	return c, nil
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, nil, sessionID, prompt, attachments...)
}

// RunAccepted implements Coordinator.
func (c *coordinator) RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, accept, sessionID, prompt, attachments...)
}

// run is the shared implementation behind Run and RunAccepted. When
// accept is non-nil it is threaded onto the SessionAgentCall as
// Accepted so sessionAgent.Run can consume the accept reservation under
// dispatchMu; when nil (the in-process/local path) no accept tracking
// applies.
func (c *coordinator) run(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	// refresh models before each run
	if err := c.UpdateModels(ctx); err != nil {
		return nil, fmt.Errorf("failed to update models: %w", err)
	}

	model := c.currentAgent.Model()
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	if !model.CatwalkCfg.SupportsImages && attachments != nil {
		// filter out image attachments
		filteredAttachments := make([]message.Attachment, 0, len(attachments))
		for _, att := range attachments {
			if att.IsText() {
				filteredAttachments = append(filteredAttachments, att)
			}
		}
		attachments = filteredAttachments
	}

	// Activate skills invoked from the command palette so their
	// instructions persist across turns, not just the turn they were
	// attached on. The palette attaches a skill's SKILL.md as a markdown
	// attachment named after the skill.
	c.activateSkillAttachments(sessionID, attachments)
	// Honor "stop <skill>" / "normal mode" requests before the turn runs
	// so the deactivated skill is not re-injected this turn.
	c.deactivateSkillsFromPrompt(sessionID, prompt)

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	callOpts := mergeCallOptions(model, providerCfg)

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		// NOTE(@andreynering): We don't return here because the event handling to ask the user to reauthenticate
		// depends on the flow below. If refresh fails, proceed with the token we have.
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "error", err)
	}

	// Coalesce per-attempt RunComplete payloads so only the final
	// outcome reaches subscribers. Without this, the first attempt's
	// failed RunComplete (unauthorized) would race ahead of the
	// retry's success, and `crush run` would exit on the stale error
	// before ever seeing the retry result. Each attempt's
	// SessionAgentCall.OnComplete hook overwrites latest; we publish
	// exactly once after retries resolve, via PublishMustDeliver, so
	// a momentarily-full subscriber buffer can't silently drop the
	// terminal event.
	var (
		latest    notify.RunComplete
		hasLatest bool
	)
	onComplete := func(rc notify.RunComplete) {
		latest = rc
		hasLatest = true
	}
	// Propagate the caller-supplied RunID (set via agent.WithRunID
	// at the HTTP boundary in backend.SendMessage) onto the
	// SessionAgentCall so the terminal RunComplete event echoes it
	// back. Both attempts in the retry chain reuse the same RunID;
	// the coalesce closure publishes the final outcome under that
	// same correlator.
	runID := RunIDFromContext(ctx)
	run := func() (*fantasy.AgentResult, error) {
		return c.currentAgent.Run(ctx, SessionAgentCall{
			SessionID:        sessionID,
			RunID:            runID,
			Prompt:           prompt,
			Attachments:      attachments,
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  callOpts.Options,
			Temperature:      callOpts.Temperature,
			TopP:             callOpts.TopP,
			TopK:             callOpts.TopK,
			FrequencyPenalty: callOpts.FrequencyPenalty,
			PresencePenalty:  callOpts.PresencePenalty,
			OnComplete:       onComplete,
			Accepted:         accept,
		})
	}
	beforeLoaded := c.skillTracker.LoadedNames()
	var result *fantasy.AgentResult
	originalErr := c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		return c.runWithStreamErrorRetry(ctx, func() error {
			var err error
			result, err = run()
			return err
		})
	})
	logTurnSkillUsage(sessionID, prompt, c.activeSkills, c.skillTracker, beforeLoaded)

	// Notify only if still unauthorized after retry — a successful
	// retry means the user doesn't need to re-authenticate.
	if originalErr != nil && c.isUnauthorized(originalErr) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}

	if hasLatest && c.runComplete != nil {
		c.runComplete.PublishMustDeliver(ctx, pubsub.UpdatedEvent, latest)
		// Signal to the dispatcher (backend.runAgent) that the
		// authoritative terminal RunComplete for this run was already
		// emitted, so it does not publish a duplicate fallback for the
		// error it is about to receive.
		MarkRunCompletePublished(ctx)
	}
	return result, originalErr
}

// effectiveReasoningEffort returns the reasoning effort to apply for provider calls.
// It prefers the user-selected effort when valid, otherwise the model default when
// valid, and finally falls back to the first configured reasoning level.
func effectiveReasoningEffort(model Model) string {
	if !model.CatwalkCfg.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatwalkCfg.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatwalkCfg.ReasoningLevels) > 0 {
		return model.CatwalkCfg.ReasoningLevels[0]
	}
	return ""
}

// googleThinkingLevel maps Crush's off/low/medium/high thinking-budget
// levels (config.ThinkingBudgetLevels) to Gemini 3+'s ThinkingLevel enum,
// which uses distinct uppercase names (google.ThinkingLevelLow, etc; see
// charm.land/fantasy/providers/google.ThinkingLevel). "off" has no
// disabling equivalent in this enum -- unlike the token-budget path used
// by Gemini 2.x, where a zero budget disables thinking -- so it maps to
// "" (unset), leaving the model's own default thinking behavior in
// place.
func googleThinkingLevel(level string) string {
	switch level {
	case "low":
		return google.ThinkingLevelLow
	case "medium":
		return google.ThinkingLevelMedium
	case "high":
		return google.ThinkingLevelHigh
	default:
		return ""
	}
}

func getProviderOptions(model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.CatwalkCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal([]byte(got), &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	shouldSetEffort := model.CatwalkCfg.CanReason &&
		reasoningEffort != "" &&
		slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort)

	switch providerCfg.Type {
	case openai.Name, azure.Name:
		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
		if openai.IsResponsesModel(model.CatwalkCfg.ID) {
			if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) {
				mergedOptions["reasoning_summary"] = "auto"
				mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
			}
			parsed, err := openai.ParseResponsesOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			} else {
				slog.Warn("Failed to parse provider options", "provider", openai.Name, "error", err)
			}
		} else {
			parsed, err := openai.ParseOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			} else {
				slog.Warn("Failed to parse provider options", "provider", openai.Name, "error", err)
			}
		}
	case anthropic.Name, bedrock.Name:
		var (
			_, hasEffort = mergedOptions["effort"]
			_, hasThink  = mergedOptions["thinking"]
			extraBody    = make(map[string]any)
		)

		switch providerCfg.ID {
		case string(catwalk.InferenceProviderAlibabaSingapore):
			switch {
			case !hasEffort && shouldSetEffort:
				extraBody["reasoning_effort"] = reasoningEffort
			case !hasThink && model.CatwalkCfg.CanReason:
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}
			mergedOptions["extra_body"] = extraBody

		default:
			switch {
			case !hasEffort && shouldSetEffort:
				mergedOptions["effort"] = reasoningEffort
			case !hasThink:
				if budget := config.ThinkingBudgetTokens(model.ModelCfg.ReasoningEffort); budget > 0 {
					mergedOptions["thinking"] = map[string]any{"budget_tokens": budget}
				} else if model.ModelCfg.ReasoningEffort == "" && model.ModelCfg.Think {
					// Legacy boolean toggle with no discrete level
					// selected yet: preserve prior default budget.
					mergedOptions["thinking"] = map[string]any{"budget_tokens": defaultLegacyThinkBudgetTokens}
				}
			}
		}

		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err == nil {
			options[anthropic.Name] = parsed
		} else {
			slog.Warn("Failed to parse provider options", "provider", anthropic.Name, "error", err)
		}

	case openrouter.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := openrouter.ParseOptions(mergedOptions)
		if err == nil {
			options[openrouter.Name] = parsed
		} else {
			slog.Warn("Failed to parse provider options", "provider", openrouter.Name, "error", err)
		}
	case vercel.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := vercel.ParseOptions(mergedOptions)
		if err == nil {
			options[vercel.Name] = parsed
		} else {
			slog.Warn("Failed to parse provider options", "provider", vercel.Name, "error", err)
		}
	case google.Name:
		_, hasReasoning := mergedOptions["thinking_config"]
		if !hasReasoning && model.CatwalkCfg.CanReason {
			level := model.ModelCfg.ReasoningEffort
			if level == "" && model.ModelCfg.Think {
				// Legacy boolean toggle with no discrete level selected
				// yet: preserve the old always-on behavior.
				level = defaultLegacyThinkLevel
			}
			if strings.HasPrefix(model.CatwalkCfg.ID, "gemini-2") {
				// Gemini 2.x controls thinking via a numeric token
				// budget, the same shape Anthropic uses.
				if level == "off" {
					mergedOptions["thinking_config"] = map[string]any{
						"thinking_budget":  0,
						"include_thoughts": false,
					}
				} else if budget := config.ThinkingBudgetTokens(level); budget > 0 {
					mergedOptions["thinking_config"] = map[string]any{
						"thinking_budget":  budget,
						"include_thoughts": true,
					}
				}
			} else if lvl := googleThinkingLevel(level); lvl != "" {
				// Gemini 3+ controls thinking via a discrete level
				// instead of a token budget.
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_level":   lvl,
					"include_thoughts": true,
				}
			}
		}
		parsed, err := google.ParseOptions(mergedOptions)
		if err == nil {
			options[google.Name] = parsed
		} else {
			slog.Warn("Failed to parse provider options", "provider", google.Name, "error", err)
		}
	case openaicompat.Name, hyper.Name:
		extraBody := make(map[string]any)

		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			switch providerCfg.ID {
			case string(catwalk.InferenceProviderIoNet):
				extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
			default:
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		}

		// "reasoning effort" is a standard OpenAI field, but "thinking" is not.
		// Setting it in the right way for each provider.
		// TODO: Abstract this in Fantasy somehow?
		// TODO: Allow custom providers to specify how to set this?
		switch providerCfg.ID {
		case hyper.Name:
			extraBody["thinking"] = model.ModelCfg.Think
		case string(catwalk.InferenceProviderIoNet):
			if _, ok := extraBody["reasoning"]; !ok && model.CatwalkCfg.CanReason {
				if model.ModelCfg.Think {
					extraBody["reasoning"] = map[string]string{"effort": "medium"}
				} else {
					extraBody["reasoning"] = map[string]string{"effort": "none"}
				}
			}
		case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
			if model.ModelCfg.Think || reasoningEffort != "" {
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
			} else {
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			}
		case string(catwalk.InferenceProviderFireworks):
			// NOTE: Fireworks break if we set both `reasoning_effort` and `thinking`.
			if reasoningEffort == "" {
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}
		case string(catwalk.InferenceProviderAlibabaSingapore):
			if model.CatwalkCfg.CanReason {
				extraBody["enable_thinking"] = model.ModelCfg.Think
			}
		}

		mergedOptions["extra_body"] = extraBody

		parsed, err := openaicompat.ParseOptions(mergedOptions)
		if err == nil {
			options[openaicompat.Name] = parsed
		} else {
			slog.Warn("Failed to parse provider options", "provider", openaicompat.Name, "error", err)
		}
	default:
		// Known custom providers (litellm, ollama, omlx) are
		// openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			parsed, err := openaicompat.ParseOptions(mergedOptions)
			if err == nil {
				options[openaicompat.Name] = parsed
			} else {
				slog.Warn("Failed to parse provider options", "provider", openaicompat.Name, "error", err)
			}
		}
	}

	return options
}

// callParams holds the resolved provider options and sampling parameters
// for a single agent call, as computed by mergeCallOptions. Using a named
// struct (rather than a positional tuple of same-typed values) avoids a
// silent transposition bug between Temperature, TopP, FrequencyPenalty,
// and PresencePenalty.
type callParams struct {
	Options          fantasy.ProviderOptions
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
}

func mergeCallOptions(model Model, cfg config.ProviderConfig) callParams {
	return callParams{
		Options:          getProviderOptions(model, cfg),
		Temperature:      cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature),
		TopP:             cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP),
		TopK:             cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK),
		FrequencyPenalty: cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty),
		PresencePenalty:  cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty),
	}
}

func (c *coordinator) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	large, small, err := c.buildAgentModels(ctx, isSubAgent)
	if err != nil {
		return nil, err
	}
	return c.newTaskAgent(ctx, prompt, agent, isSubAgent, large, small), nil
}

// buildAgentWithModel builds a task agent whose primary (run) model is the
// given selected model instead of the configured large model. Sub-agents
// run on their primary model (see runSubAgent, which reads Agent.Model()),
// so this is how the "agent" tool dispatches a task on a caller-chosen
// model. The small model stays the configured one (used for summaries).
func (c *coordinator) buildAgentWithModel(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, primaryCfg config.SelectedModel) (SessionAgent, error) {
	primary, err := c.buildModelFromSelected(ctx, primaryCfg, true)
	if err != nil {
		return nil, err
	}
	_, small, err := c.buildAgentModels(ctx, true)
	if err != nil {
		return nil, err
	}
	return c.newTaskAgent(ctx, prompt, agent, true, primary, small), nil
}

// newTaskAgent assembles a SessionAgent with the given primary and small
// models, wiring up its system prompt and tools asynchronously. It is the
// shared constructor behind buildAgent and buildAgentWithModel.
func (c *coordinator) newTaskAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool, primary, small Model) SessionAgent {
	primaryProviderCfg, _ := c.cfg.Config().Providers.Get(primary.ModelCfg.Provider)
	recallMemories, resetMemoryShown := c.buildMemoryRecall(isSubAgent, small)
	result := NewSessionAgent(SessionAgentOptions{
		LargeModel:           primary,
		SmallModel:           small,
		SystemPromptPrefix:   primaryProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                nil,
		Notify:               c.notify,
		RunComplete:          c.runComplete,
		ActiveSkillsFor:      c.activeSkillsInjection,
		RecallMemories:       recallMemories,
		CompressToolOutput:   c.buildCompressToolOutput(isSubAgent),
		OnSummarized: func(sessionID string) {
			c.loadedSkills.Bump(sessionID)
			if resetMemoryShown != nil {
				resetMemoryShown(sessionID)
			}
		},
	})

	// Arm the readiness barrier for the two async init tasks below so a
	// turn dispatched before they finish (e.g. a freshly built task
	// sub-agent invoked immediately) waits in Run rather than executing
	// tool-less. See sessionAgent.ArmReady/WaitReady.
	markReady := result.ArmReady(2)

	c.readyWg.Go(func() error {
		systemPrompt, err := prompt.Build(ctx, primary.Model.Provider(), primary.Model.Model(), c.cfg)
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		markReady()
		return nil
	})

	c.readyWg.Go(func() error {
		tools, err := c.buildTools(ctx, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(tools)
		markReady()
		return nil
	})

	return result
}

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	var allTools []fantasy.AgentTool
	if slices.Contains(agent.AllowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agentTool)
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, nil)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agenticFetchTool)
	}

	if slices.Contains(agent.AllowedTools, WorkflowToolName) {
		workflowTool, err := c.workflowTool(ctx, nil)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, workflowTool)
	}

	// Get the model name for the agent
	modelID := ""
	if modelCfg, ok := c.cfg.Config().Models[agent.Model]; ok {
		if model := c.cfg.Config().GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelID = model.ID
		}
	}

	logFile := filepath.Join(c.cfg.Config().Options.DataDirectory, "logs", "crush.log")

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := c.cfg.Config().Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}

	editMode := c.cfg.Config().Options.EditMode

	allTools = append(
		allTools,
		tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.Attribution, modelID),
		tools.NewCrushInfoTool(c.cfg, c.lspManager, c.allSkills, c.activeSkills, c.skillTracker),
		tools.NewCrushLogsTool(logFile),
		tools.NewJobOutputTool(),
		tools.NewJobKillTool(),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil),
	)

	// The editing tool family depends on the configured edit mode. Hashline
	// mode registers a single line-anchored Edit tool (natively multi-file);
	// string mode keeps the exact-match Edit + MultiEdit pair. Both register
	// under the "Edit" name so the model always calls the same tool. Kept in
	// the original list position so string-mode requests are unchanged.
	if editMode == config.EditModeHashline {
		allTools = append(
			allTools,
			tools.NewHashlineEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.snapshots, tsblock.New(), c.cfg.WorkingDir(), c.cfg.Config().Options.ValidateEditSyntax()),
		)
	} else {
		allTools = append(
			allTools,
			tools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
			tools.NewMultiEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		)
	}

	allTools = append(
		allTools,
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewGlobTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Glob),
		tools.NewGrepTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep),
		tools.NewAstGrepTool(c.cfg.WorkingDir()),
	)
	if c.memory != nil && c.cfg.Config().Options.MemoryEnabled() {
		allTools = append(
			allTools,
			tools.NewRememberTool(c.memory, c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.MemoryMaxPerScope()),
			tools.NewRecallTool(c.memory, c.cfg.WorkingDir()),
			tools.NewForgetTool(c.memory, c.permissions, c.cfg.WorkingDir()),
		)
	}
	allTools = append(
		allTools,
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls),
		tools.NewSourcegraphTool(nil),
		c.scheduleCronTool(),
		c.scheduleWakeupTool(),
		c.scheduleListTool(),
		c.scheduleCancelTool(),
		c.agentListTool(),
		c.agentProgressTool(),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, c.skillTracker, c.loadedSkills, editMode, c.snapshots, c.cfg.Config().Options.SummarizeReads(), c.cfg.Config().Options.SummarizeMinLines(), c.cfg.Config().Options.SummarizeBudget(), c.cfg.WorkingDir(), c.cfg.Config().Options.SkillsPaths...),
		tools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewSkillTool(c.activeSkills, c.loadedSkills),
	)

	// Only the main (non-sub-agent) coder ever has compressed tool
	// outputs, so only it needs a way to retrieve the original.
	if !isSubAgent && c.compressdMgr != nil {
		allTools = append(allTools, tools.NewRetrieveFullOutputTool(c.retrieveStore))
	}

	// Add LSP tools if user has configured LSPs or auto_lsp is enabled (nil or true).
	if len(c.cfg.Config().LSP) > 0 || c.cfg.Config().Options.AutoLSP == nil || *c.cfg.Config().Options.AutoLSP {
		allTools = append(allTools, tools.NewDiagnosticsTool(c.lspManager), tools.NewReferencesTool(c.lspManager), tools.NewLSPRestartTool(c.lspManager))
	}

	if len(c.cfg.Config().MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(c.cfg, c.permissions),
			tools.NewReadMCPResourceTool(c.cfg, c.permissions),
		)
	}

	var filteredTools []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(agent.AllowedTools, tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}

	for _, tool := range tools.GetMCPTools(c.permissions, c.cfg, c.cfg.WorkingDir()) {
		if agent.AllowedMCP == nil {
			// No MCP restrictions
			filteredTools = append(filteredTools, tool)
			continue
		}
		if len(agent.AllowedMCP) == 0 {
			// No MCPs allowed
			slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
			break
		}

		for mcp, tools := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(tools) == 0 || slices.Contains(tools, tool.MCPToolName()) {
				filteredTools = append(filteredTools, tool)
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	// A writable agent is one whose tool set contains mutating tools
	// (bash, edit, write, download). Read-only sub-agents have none.
	writable := false
	for _, tool := range filteredTools {
		if permission.PlanModeBlocksTool(tool.Info().Name) {
			writable = true
			break
		}
	}

	// Wrap tools with hook interception. The top-level agent always fires
	// PreToolUse hooks; read-only sub-agents skip them to avoid firing the
	// user's hooks N times per delegated turn. Writable sub-agents DO fire
	// them, so the user's safety guardrails (e.g. block dangerous shell
	// commands) still apply to the delegated mutating calls.
	filteredTools = wrapToolsWithHooks(filteredTools, hookRunner, isSubAgent && !writable)

	// Gate mutating tools on plan mode. This applies to every agent,
	// including writable sub-agents, so a sub-agent dispatched while plan
	// mode is active cannot bypass it. wrapToolsWithPlanMode only wraps
	// mutating tools, so it is a no-op for read-only sub-agents.
	filteredTools = wrapToolsWithPlanMode(filteredTools, c.permissions)

	// Tag tool-originated errors so a tool failure that halts the run is
	// reported distinctly instead of as a generic provider error. This
	// wraps the hook and plan-mode decorators so it also captures their
	// errors.
	filteredTools = wrapToolsWithErrorTagging(filteredTools)

	// Guarantee that a run cancellation always returns control to the user,
	// even if a tool ignores context cancellation or is wedged in a
	// blocking syscall. This must be the outermost wrapper so it races the
	// whole decorator chain (hooks, plan mode, error tagging, tool) against
	// the run context.
	filteredTools = wrapToolsWithCancellation(filteredTools)

	return filteredTools, nil
}

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	largeModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeLarge]
	if !ok {
		return Model{}, Model{}, errLargeModelNotSelected
	}
	smallModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}

	large, err := c.buildModelFromSelected(ctx, largeModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}
	// The small model always runs as a sub-agent (summaries, titles).
	small, err := c.buildModelFromSelected(ctx, smallModelCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}
	return large, small, nil
}

// buildModelFromSelected constructs a runnable Model from a selected model
// configuration: it resolves the provider, builds the provider client,
// looks up the catwalk metadata, and creates the language model. It is the
// shared path used for the configured large/small models and for ad-hoc
// models the "agent" tool selects per task.
func (c *coordinator) buildModelFromSelected(ctx context.Context, modelCfg config.SelectedModel, isSubAgent bool) (Model, error) {
	providerCfg, ok := c.cfg.Config().Providers.Get(modelCfg.Provider)
	if !ok {
		return Model{}, errModelProviderNotConfigured
	}

	provider, err := c.buildProvider(providerCfg, modelCfg, isSubAgent)
	if err != nil {
		return Model{}, err
	}

	var catwalkModel *catwalk.Model
	for _, m := range providerCfg.Models {
		if m.ID == modelCfg.Model {
			catwalkModel = &m
			break
		}
	}
	if catwalkModel == nil {
		return Model{}, errModelNotFound
	}

	modelID := modelCfg.Model
	if modelCfg.Provider == openrouter.Name && isExactoSupported(modelID) {
		modelID += ":exacto"
	}

	languageModel, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, err
	}

	return Model{
		Model:      languageModel,
		CatwalkCfg: *catwalkModel,
		ModelCfg:   modelCfg,
		FlatRate:   providerCfg.FlatRate,
	}, nil
}

// debugHTTPClient returns an HTTP client that logs request/response
// traffic when debug mode is enabled in config, or nil otherwise. It
// centralizes the "opt into a debug HTTP client" check that every
// build*Provider method otherwise repeated individually.
func (c *coordinator) debugHTTPClient() *http.Client {
	if c.cfg.Config().Options.Debug {
		return log.NewHTTPClient()
	}
	return nil
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string) (fantasy.Provider, error) {
	var opts []anthropic.Option

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = apiKey
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = "Bearer " + apiKey
	case apiKey != "":
		// X-Api-Key header
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	// Route all Anthropic traffic through a transport that splits the
	// leading Claude Code identity line into its own system block, which
	// the subscription-OAuth endpoint requires (see cc_system_split.go).
	// It is a no-op for non-Claude-Code system prompts.
	var base http.RoundTripper = http.DefaultTransport
	if hc := c.debugHTTPClient(); hc != nil {
		base = hc.Transport
	}
	// For the native Claude Code subscription provider, inject a fresh
	// OAuth bearer token (read+refreshed from ~/.claude/.credentials.json)
	// on every request. No api_key/shell helper is needed.
	oauthSubscription := providerID == claudecode.ProviderID
	if oauthSubscription {
		os.Setenv("ANTHROPIC_API_KEY", "")
		base = &claudecode.AuthTransport{Base: base, Source: claudecode.DefaultSource()}
	}
	// The subscription-OAuth endpoint requires the Claude Code identity as
	// a discrete first system block. injectIdentity makes the transport
	// guarantee that for every request (titles, summaries, sub-agents — not
	// just the coder prompt). It is off for plain Anthropic API keys so we
	// never spoof the identity onto non-subscription traffic.
	httpClient := &http.Client{Transport: &ccSystemSplitTransport{base: base, injectIdentity: oauthSubscription}}
	opts = append(opts, anthropic.WithHTTPClient(httpClient))
	return anthropic.New(opts...)
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	if hc := c.debugHTTPClient(); hc != nil {
		opts = append(opts, openai.WithHTTPClient(hc))
	}
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (c *coordinator) buildOpenrouterProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if hc := c.debugHTTPClient(); hc != nil {
		opts = append(opts, openrouter.WithHTTPClient(hc))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (c *coordinator) buildVercelProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if hc := c.debugHTTPClient(); hc != nil {
		opts = append(opts, vercel.WithHTTPClient(hc))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	}
	if httpClient == nil {
		httpClient = c.debugHTTPClient()
	}
	if httpClient != nil {
		opts = append(opts, openaicompat.WithHTTPClient(httpClient))
	}

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (c *coordinator) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	if hc := c.debugHTTPClient(); hc != nil {
		opts = append(opts, azure.WithHTTPClient(hc))
	}
	if options == nil {
		options = make(map[string]string)
	}
	if apiVersion, ok := options["apiVersion"]; ok {
		opts = append(opts, azure.WithAPIVersion(apiVersion))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

func (c *coordinator) buildBedrockProvider(apiKey string, headers map[string]string, providerID string) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if hc := c.debugHTTPClient(); hc != nil {
		opts = append(opts, bedrock.WithHTTPClient(hc))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}

	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}

	switch providerID {
	case string(catwalk.InferenceProviderBedrockEurope):
		opts = append(opts, bedrock.WithRegion("eu-west-1"))
	default:
		opts = append(opts, bedrock.WithRegion("us-east-1"))
	}

	return bedrock.New(opts...)
}

func (c *coordinator) buildGoogleProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if hc := c.debugHTTPClient(); hc != nil {
		opts = append(opts, google.WithHTTPClient(hc))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) buildGoogleVertexProvider(headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{}
	if hc := c.debugHTTPClient(); hc != nil {
		opts = append(opts, google.WithHTTPClient(hc))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

// buildCodexProvider builds the OpenAI Codex (ChatGPT subscription)
// provider. It talks the OpenAI Responses API against the ChatGPT backend
// and injects the subscription bearer, chatgpt-account-id, and Codex beta
// headers on every request via codex.AuthTransport.
func (c *coordinator) buildCodexProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	var base http.RoundTripper = http.DefaultTransport
	if hc := c.debugHTTPClient(); hc != nil {
		base = hc.Transport
	}

	return codex.NewProvider(codex.ProviderOptions{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Headers: headers,
		HTTPClient: &http.Client{
			Transport: codex.NewAuthTransport(base, apiKey),
		},
	})
}

// buildGeminiCliProvider builds the Gemini CLI (Cloud Code Assist)
// provider, also reused for the experimental Antigravity provider since
// both share the same backend and wire format (see
// docs/antigravity-cli-oauth-findings.md). Requests are driven through the
// standard fantasy google provider but rewritten onto the Code Assist wire
// format (and signed with the subscription bearer + discovered project
// id) by geminicli.WireTransport.
func (c *coordinator) buildGeminiCliProvider(apiKey string, headers, oauthExtra map[string]string, identity geminicli.Identity) (fantasy.Provider, error) {
	projectID := ""
	if oauthExtra != nil {
		projectID = oauthExtra["project_id"]
	}
	var base http.RoundTripper = http.DefaultTransport
	if hc := c.debugHTTPClient(); hc != nil {
		base = hc.Transport
	}
	httpClient := &http.Client{Transport: &geminicli.WireTransport{
		Base:        base,
		AccessToken: apiKey,
		ProjectID:   projectID,
		Identity:    identity,
	}}
	opts := []google.Option{
		// genai.NewClient hard-requires a non-empty APIKey whenever the
		// backend isn't Vertex AI, even when Credentials/skipAuth are
		// set (see google.golang.org/genai@v1.61.0 client.go), so
		// WithSkipAuth alone left buildProvider always failing with
		// "api key is required for Google AI backend" for both Gemini
		// CLI and Antigravity. Requesting the Vertex AI backend avoids
		// that check entirely once a custom BaseURL is set (genai's own
		// project/location auth requirement is waived for a custom
		// BaseURL) -- and it doesn't matter that these placeholder
		// project/location values aren't real: geminicli.WireTransport
		// discards genai's own request URL and body outright and
		// rebuilds both from scratch, and it also injects the real
		// bearer token, so genai's own auth path (already bypassed by
		// the custom HTTPClient below) never runs.
		google.WithVertex("unused-wiretransport-handles-auth", "global"),
		google.WithBaseURL(geminicli.BaseURL),
		google.WithSkipAuth(true),
		google.WithHTTPClient(httpClient),
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func (c *coordinator) buildProvider(providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && c.isAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	apiKey, _ := c.cfg.Resolve(providerCfg.APIKey)
	baseURL, _ := c.cfg.Resolve(providerCfg.BaseURL)

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if opencodeMessagesModels[model.Model] {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID)
		}
	case codex.ProviderID:
		return c.buildCodexProvider(baseURL, apiKey, headers)
	case geminicli.ProviderID:
		return c.buildGeminiCliProvider(apiKey, headers, providerCfg.OAuthExtra, geminicli.GeminiCLIIdentity)
	case antigravity.ProviderID:
		return c.buildGeminiCliProvider(apiKey, headers, providerCfg.OAuthExtra, antigravity.Identity)
	}

	switch providerCfg.Type {
	case openai.Name:
		return c.buildOpenaiProvider(baseURL, apiKey, headers)
	case anthropic.Name:
		return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID)
	case openrouter.Name:
		return c.buildOpenrouterProvider(baseURL, apiKey, headers)
	case vercel.Name:
		return c.buildVercelProvider(baseURL, apiKey, headers)
	case azure.Name:
		return c.buildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams)
	case bedrock.Name:
		return c.buildBedrockProvider(apiKey, headers, providerCfg.ID)
	case google.Name:
		return c.buildGoogleProvider(baseURL, apiKey, headers)
	case "google-vertex":
		return c.buildGoogleVertexProvider(headers, providerCfg.ExtraParams)
	case openaicompat.Name, hyper.Name:
		switch providerCfg.ID {
		case hyper.Name:
			baseURL = hyper.BaseURL() + "/v1"
			headers["x-crush-id"] = event.GetID()
		case string(catwalk.InferenceProviderZAI):
			if providerCfg.ExtraBody == nil {
				providerCfg.ExtraBody = map[string]any{}
			}
			providerCfg.ExtraBody["tool_stream"] = true
		}
		return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
	default:
		// Known custom providers (litellm, ollama, omlx) are
		// openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
}

// BeginAccepted reserves an accept slot for sessionID on the active
// agent and returns the ownership handle. It is the fire-and-forget
// dispatch path's only way to mark a run as accepted-but-not-yet-active
// so a cancel arriving before the run registers in activeRequests is not
// lost.
func (c *coordinator) BeginAccepted(sessionID string) *AcceptedRun {
	return c.currentAgent.BeginAccepted(sessionID)
}

func (c *coordinator) Cancel(sessionID string) {
	c.currentAgent.Cancel(sessionID)
}

func (c *coordinator) CancelKeepQueue(sessionID string) {
	c.currentAgent.CancelKeepQueue(sessionID)
}

// CancelAll stops every run the coordinator knows about: the main
// coder agent, every task/sub-agent instance dispatched via the
// "agent" tool (each mode/model combination runs on its own
// [SessionAgent], separate from currentAgent), every background
// workflow, and every scheduled task. Called on app shutdown so
// nothing is left running headless after the process exits — without
// this, an in-flight sub-agent, workflow, or scheduled task is simply
// abandoned, leaving its session with an unfinished tool call that
// the UI then displays as perpetually running on the next resume.
func (c *coordinator) CancelAll() {
	c.currentAgent.CancelAll()
	for taskAgent := range c.taskAgents.Seq() {
		if taskAgent != nil {
			taskAgent.CancelAll()
		}
	}
	for _, wf := range c.workflows.list() {
		c.workflows.cancel(wf.SessionID)
	}
	for _, sched := range c.schedules.listAll() {
		c.schedules.stop(sched.ID, "canceled")
	}
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
}

func (c *coordinator) Model() Model {
	return c.currentAgent.Model()
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	// build the models again so we make sure we get the latest config
	large, small, err := c.buildAgentModels(ctx, false)
	if err != nil {
		return err
	}
	c.currentAgent.SetModels(large, small)

	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return errCoderAgentNotConfigured
	}

	tools, err := c.buildTools(ctx, agentCfg, false)
	if err != nil {
		return err
	}
	c.currentAgent.SetTools(tools)
	return nil
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent.QueuedPromptsList(sessionID)
}

func (c *coordinator) Summarize(ctx context.Context, sessionID string) error {
	providerCfg, ok := c.cfg.Config().Providers.Get(c.currentAgent.Model().ModelCfg.Provider)
	if !ok {
		return errModelProviderNotConfigured
	}

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "error", err)
	}

	summarize := func() error {
		return c.currentAgent.Summarize(ctx, sessionID, getProviderOptions(c.currentAgent.Model(), providerCfg))
	}

	return c.runWithUnauthorizedRetry(ctx, providerCfg, summarize)
}

// GenerateTitle generates a session title using the current agent. It
// returns errCoderAgentNotConfigured if no agent is configured, matching
// RegenerateTitle's behavior for the identical guard.
func (c *coordinator) GenerateTitle(ctx context.Context, sessionID, prompt string) error {
	if c.currentAgent == nil {
		return errCoderAgentNotConfigured
	}
	c.currentAgent.GenerateTitle(ctx, sessionID, prompt)
	return nil
}

// RegenerateTitle re-runs AI title generation for a session using the whole
// conversation as context (not just the first message), so the title reflects
// where the session actually went.
func (c *coordinator) RegenerateTitle(ctx context.Context, sessionID string) error {
	if c.currentAgent == nil {
		return errCoderAgentNotConfigured
	}
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}
	prompt := conversationText(msgs)
	if prompt == "" {
		return errors.New("session has no content to generate a title from")
	}
	c.currentAgent.GenerateTitle(ctx, sessionID, prompt)
	return nil
}

// SendToSubAgent implements Coordinator. It steers a running sub-agent
// turn by injecting a follow-up user prompt into its session. The
// prompt is queued and folded into the sub-agent's next step (the same
// mechanism used to steer the main session while it's busy), so the
// sub-agent picks it up mid-turn without interrupting the current
// step. If the sub-agent session is not currently busy (never started,
// or already finished), it returns errSubAgentNotRunning rather than
// starting a new independent run.
func (c *coordinator) SendToSubAgent(ctx context.Context, subAgentSessionID, prompt string) error {
	if c.taskAgents.Len() == 0 {
		return errTaskAgentNotConfigured
	}
	for taskAgent := range c.taskAgents.Seq() {
		if taskAgent == nil || !taskAgent.IsSessionBusy(subAgentSessionID) {
			continue
		}
		_, err := taskAgent.Run(ctx, SessionAgentCall{
			SessionID: subAgentSessionID,
			Prompt:    prompt,
		})
		return err
	}
	return errSubAgentNotRunning
}

// CancelSubAgent implements Coordinator.
func (c *coordinator) CancelSubAgent(subAgentSessionID string) {
	for taskAgent := range c.taskAgents.Seq() {
		if taskAgent == nil || !taskAgent.IsSessionBusy(subAgentSessionID) {
			continue
		}
		taskAgent.Cancel(subAgentSessionID)
		return
	}
}

// BackgroundNow implements Coordinator.
func (c *coordinator) BackgroundNow(sessionID string) map[tools.BackgroundKind]int {
	return tools.BackgroundNow(sessionID)
}

// SubscribeTasks implements Coordinator.
func (c *coordinator) SubscribeTasks(ctx context.Context) <-chan pubsub.Event[TaskStatusEvent] {
	return SubscribeTaskStatus(ctx)
}

// titleContextMaxChars caps how much conversation text is fed to title
// generation. The most recent text is kept (tail slice) so the title reflects
// where the session ended up, while keeping the request small and cheap.
const titleContextMaxChars = 4000

// conversationText flattens the user and assistant text of a session into a
// single string for title generation. Tool, system, and empty messages are
// skipped. The result is tail-sliced to titleContextMaxChars so recent
// context wins when the conversation is long.
func conversationText(msgs []message.Message) string {
	var b strings.Builder
	for _, msg := range msgs {
		if msg.Role != message.User && msg.Role != message.Assistant {
			continue
		}
		for _, part := range msg.Parts {
			tc, ok := part.(message.TextContent)
			if !ok || tc.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(tc.Text)
		}
	}
	text := strings.TrimSpace(b.String())
	if len(text) > titleContextMaxChars {
		text = text[len(text)-titleContextMaxChars:]
	}
	return text
}

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (c *coordinator) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig) error {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	return c.refreshOAuth2Token(ctx, providerCfg)
}

// runWithUnauthorizedRetry executes fn. If fn returns a 401 error, it
// attempts to refresh credentials and re-runs fn once. Returns the
// final error: from the retry if a retry was attempted, otherwise from
// the original run. Callers that need to notify the user on persistent
// failure should check isUnauthorized on the returned error.
func (c *coordinator) runWithUnauthorizedRetry(ctx context.Context, providerCfg config.ProviderConfig, fn func() error) error {
	err := fn()
	if err != nil && c.isUnauthorized(err) {
		if retryErr := c.retryAfterUnauthorized(ctx, providerCfg); retryErr == nil {
			return fn()
		}
	}
	return err
}

// retryAfterUnauthorized attempts to refresh credentials after receiving a 401
// and returns nil if retry should be attempted.
func (c *coordinator) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig) error {
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		return c.refreshOAuth2Token(ctx, providerCfg)
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return c.refreshAPIKeyTemplate(ctx, providerCfg)
	default:
		return nil
	}
}

func (c *coordinator) isUnauthorized(err error) bool {
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

// unclassifiedStreamErrorPrefix is the literal error text the Anthropic SDK
// produces when the SSE stream carries a mid-response "event: error" frame
// (see anthropic-sdk-go's ssestream package). fantasy's Anthropic provider
// cannot turn this into a *fantasy.ProviderError -- that requires an
// *anthropic.Error, which the SDK only produces for HTTP-level failures --
// so it reaches Crush as a bare, unclassified error that fantasy's own
// per-request retry logic never retries.
const unclassifiedStreamErrorPrefix = "received error while streaming: "

// anthropicStreamErrorEvent is the JSON payload embedded in an Anthropic
// mid-stream SSE "error" event.
type anthropicStreamErrorEvent struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// classifyUnclassifiedStreamError recognizes the subset of Anthropic
// mid-stream SSE error types that are safe to retry (transient capacity or
// rate-limit conditions) -- the same categories fantasy would treat as
// retryable had the failure come back as an HTTP status instead of an SSE
// frame. It returns ok=false for any error it does not recognize, including
// non-transient error types (e.g. invalid_request_error), so callers only
// retry when this returns ok=true.
func classifyUnclassifiedStreamError(err error) (reason string, ok bool) {
	if err == nil {
		return "", false
	}
	payload, hasPrefix := strings.CutPrefix(err.Error(), unclassifiedStreamErrorPrefix)
	if !hasPrefix {
		return "", false
	}
	var event anthropicStreamErrorEvent
	if jsonErr := json.Unmarshal([]byte(payload), &event); jsonErr != nil {
		return "", false
	}
	switch event.Error.Type {
	case "rate_limit_error":
		return "Rate limited", true
	case "overloaded_error":
		return "Overloaded", true
	case "api_error":
		return "Server error", true
	default:
		return "", false
	}
}

// runWithStreamErrorRetry executes fn, retrying with exponential backoff
// when it fails with an error fantasy did not classify as retryable but
// that Crush recognizes as a transient provider condition (see
// classifyUnclassifiedStreamError). This covers, for example, an Anthropic
// mid-stream SSE "error" event carrying rate_limit_error, overloaded_error,
// or api_error: the Anthropic SDK surfaces these as a bare Go error rather
// than a *fantasy.ProviderError, so fantasy's own per-request retry (which
// requires that type) never engages and the turn would otherwise fail on
// the very first hit -- reported to the user as a plain "Provider Error"
// with no retry at all.
//
// Retries share the same providerMaxRetries ceiling and 2s/x2 backoff shape
// as fantasy's internal retry, so the wait behaves the same from the
// user's perspective. Unlike that internal retry, this one only runs after
// the whole turn has already returned -- its assistant message is already
// finalized as an error by the time Crush sees it -- so a retry here starts
// a fresh turn rather than resuming the same in-flight message. That is the
// same trade-off already accepted by runWithUnauthorizedRetry.
func (c *coordinator) runWithStreamErrorRetry(ctx context.Context, fn func() error) error {
	return retryStreamError(ctx, fn, providerMaxRetries, 2*time.Second, 2.0)
}

// retryStreamError is the delay-parameterized implementation behind
// runWithStreamErrorRetry, split out so tests can drive the retry/backoff
// logic with a negligible delay instead of the real multi-second backoff.
func retryStreamError(ctx context.Context, fn func() error, maxRetries int, initialDelay time.Duration, backoffFactor float64) error {
	delay := initialDelay
	err := fn()
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err == nil {
			return nil
		}
		reason, retryable := classifyUnclassifiedStreamError(err)
		if !retryable {
			return err
		}
		slog.Warn("Provider stream error not classified as retryable by fantasy; retrying the turn",
			"attempt", attempt, "max_retries", maxRetries, "reason", reason, "retry_delay", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return err
		}
		delay = time.Duration(float64(delay) * backoffFactor)
		err = fn()
	}
	return err
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	if err := c.cfg.RefreshOAuthToken(ctx, config.ScopeGlobal, providerCfg.ID); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

func (c *coordinator) refreshAPIKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig) error {
	newAPIKey, err := c.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	providerCfg.APIKey = newAPIKey
	c.cfg.Config().Providers.Set(providerCfg.ID, providerCfg)

	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	AgentMessageID string
	ToolCallID     string
	Prompt         string
	SessionTitle   string
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
	// ResumeSessionID, when set, continues an existing agent-tool
	// session instead of creating a new one (see resumeAgentToolSession
	// for the validation this goes through). SessionSetup and
	// SessionTitle are ignored on resume: the session already exists
	// and was set up on its first run.
	ResumeSessionID string
	// ToolName identifies the dispatching tool ("agent",
	// "agentic_fetch", or "Workflow") for the subAgentRegistry entry.
	ToolName string
	// Label is a short human-readable description of the task,
	// recorded in the subAgentRegistry entry for AgentList/
	// AgentProgress. Defaults to a truncated Prompt if empty.
	Label string
	// Background, when set, detaches the sub-agent immediately instead
	// of blocking the tool call until it finishes: runSubAgent returns
	// right after starting the run, and the eventual result is queued
	// back into the parent session as a follow-up message (see
	// completeSubAgentBackgrounded), exactly as if Ctrl+B had
	// backgrounded it mid-flight.
	Background bool
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session (or continues an existing one, see
// subAgentParams.ResumeSessionID), runs the agent with the given prompt, and
// propagates the cost to the parent session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	var subSession session.Session
	var err error
	if params.ResumeSessionID != "" {
		subSession, err = c.resumeAgentToolSession(ctx, params.ResumeSessionID, params.SessionID)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	} else {
		agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
		subSession, err = c.sessions.CreateTaskSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
		}
		// Call session setup function if provided
		if params.SessionSetup != nil {
			params.SessionSetup(subSession.ID)
		}
	}

	// Get model configuration
	model := params.Agent.Model()
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}

	label := cmp.Or(params.Label, truncateForList(params.Prompt, 80))
	c.subAgents.register(SubAgentStatus{
		SessionID:       subSession.ID,
		ParentSessionID: params.SessionID,
		ToolCallID:      params.ToolCallID,
		ToolName:        params.ToolName,
		Label:           label,
		Provider:        model.ModelCfg.Provider,
		Model:           model.ModelCfg.Model,
		StartedAt:       time.Now(),
		State:           SubAgentRunning,
	})

	// bgCtx detaches the actual run from the tool call's own context up
	// front: if Ctrl+B backgrounds this sub-agent, it must keep running
	// after runSubAgent returns, which a context tied to the (about to be
	// discarded) tool-call context cannot do. A genuine turn cancellation
	// (Esc) is re-propagated explicitly below via bgCancel, preserving
	// today's "Esc kills everything" behavior for the common,
	// non-backgrounded case.
	bgCtx, bgCancel := context.WithCancel(context.WithoutCancel(ctx))

	resultCh := make(chan subAgentRunOutcome, 1)
	go func() {
		var result *fantasy.AgentResult
		runErr := c.runWithUnauthorizedRetry(bgCtx, providerCfg, func() error {
			var innerErr error
			result, innerErr = params.Agent.Run(bgCtx, SessionAgentCall{
				SessionID:        subSession.ID,
				Prompt:           params.Prompt,
				MaxOutputTokens:  maxTokens,
				ProviderOptions:  getProviderOptions(model, providerCfg),
				Temperature:      model.ModelCfg.Temperature,
				TopP:             model.ModelCfg.TopP,
				TopK:             model.ModelCfg.TopK,
				FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
				PresencePenalty:  model.ModelCfg.PresencePenalty,
				NonInteractive:   true,
			})
			return innerErr
		})
		resultCh <- subAgentRunOutcome{result: result, err: runErr}
	}()

	if params.Background {
		// Detach immediately -- the caller asked to background this
		// sub-agent from the start rather than waiting for Ctrl+B, so
		// skip the blocking select entirely.
		go func() {
			out := <-resultCh
			bgCancel()
			c.completeSubAgentBackgrounded(params, subSession, model, out)
		}()
		return fantasy.NewTextResponse(fmt.Sprintf(
			"Started the sub-agent in the background (session %s). I'll follow up in this conversation once it finishes -- check on it any time with AgentProgress(session_id=%q).",
			subSession.ID, subSession.ID,
		)), nil
	}

	trigger, unregisterBackground := tools.RegisterBackground(params.SessionID, params.ToolCallID, tools.BackgroundKindSubAgent)
	defer unregisterBackground()

	select {
	case out := <-resultCh:
		bgCancel()
		return c.completeSubAgentSync(ctx, params, subSession, model, out)

	case <-trigger.C():
		// Detach: return immediately and let the run finish in the
		// background. bgCtx is deliberately left alone here (not
		// canceled) -- that is the whole point of backgrounding.
		go func() {
			out := <-resultCh
			bgCancel()
			c.completeSubAgentBackgrounded(params, subSession, model, out)
		}()
		return fantasy.NewTextResponse(fmt.Sprintf(
			"Moved the sub-agent to the background (session %s). I'll follow up in this conversation once it finishes -- check on it any time with AgentProgress(session_id=%q).",
			subSession.ID, subSession.ID,
		)), nil

	case <-ctx.Done():
		// The parent turn's own context was canceled (e.g. Esc) before
		// Ctrl+B fired -- propagate the cancellation into the sub-agent
		// (via bgCancel) rather than letting it run forever detached, and
		// wait for it to actually unwind before returning, matching the
		// blocking behavior a plain synchronous call already had.
		bgCancel()
		out := <-resultCh
		reason := ctx.Err().Error()
		if out.err != nil {
			reason = out.err.Error()
		}
		c.subAgents.finish(subSession.ID, SubAgentFailed, reason)
		c.lingerSubAgentRemoval(subSession.ID)
		return fantasy.ToolResponse{}, ctx.Err()
	}
}

// subAgentRunOutcome carries a sub-agent turn's result off the goroutine
// that runs it, to whichever select case in runSubAgent (or the detached
// backgrounded-completion goroutine) ends up consuming it.
type subAgentRunOutcome struct {
	result *fantasy.AgentResult
	err    error
}

// lingerSubAgentRemoval keeps a finished sub-agent visible in the
// registry (and thus AgentList) for subAgentLingerAfterFinish before
// dropping it, mirroring workflowLingerAfterFinish.
func (c *coordinator) lingerSubAgentRemoval(sessionID string) {
	go func() {
		time.Sleep(subAgentLingerAfterFinish)
		c.subAgents.remove(sessionID)
	}()
}

// completeSubAgentSync finishes a sub-agent turn that ran to completion
// (or failed) before the tool call itself returned -- the common,
// non-backgrounded case. It formats the same response a direct,
// unbackgrounded runSubAgent always returned.
func (c *coordinator) completeSubAgentSync(ctx context.Context, params subAgentParams, subSession session.Session, model Model, out subAgentRunOutcome) (fantasy.ToolResponse, error) {
	c.notifyIfUnauthorized(model, out.err)
	if out.err != nil {
		c.subAgents.finish(subSession.ID, SubAgentFailed, out.err.Error())
		c.lingerSubAgentRemoval(subSession.ID)
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Failed to generate response: %s\n\nThe sub-agent's progress up to this point is preserved in session %q. Retry the agent tool with resume_session_id=%q to continue from where it left off instead of starting over.",
			out.err, subSession.ID, subSession.ID,
		)), nil
	}

	// Update parent session cost on a best-effort basis. A failure here must
	// not discard the sub-agent output that was already produced.
	c.finalizeSubAgentCost(ctx, subSession.ID, params.SessionID)

	output := subAgentOutput(out.result)

	// Defense in depth: a healthy sub-agent that did real work makes tool
	// calls. A run that made ZERO tool calls yet emitted raw tool-call
	// markup in its text is degenerate -- the model narrated tool calls as
	// prose instead of executing them (typically when it ran effectively
	// tool-less) and "reported" fabricated work. Surface it as a retryable
	// error instead of returning the fake report to the parent.
	if subAgentToolCallCount(out.result) == 0 && looksLikeNarratedToolCalls(output) {
		c.subAgents.finish(subSession.ID, SubAgentFailed, "sub-agent narrated tool calls as text")
		c.lingerSubAgentRemoval(subSession.ID)
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"The sub-agent made no tool calls and emitted tool-call syntax as plain text, so it did no real work. This usually clears on a fresh attempt. Retry the agent tool with resume_session_id=%q to continue in the same session.",
			subSession.ID,
		)), nil
	}

	c.subAgents.finish(subSession.ID, SubAgentDone, "")
	c.lingerSubAgentRemoval(subSession.ID)
	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output."), nil
	}
	return fantasy.NewTextResponse(output), nil
}

// completeSubAgentBackgrounded finishes a sub-agent turn that was detached
// via Ctrl+B, after runSubAgent has already returned its "moved to the
// background" response. Since there is no tool call left to return a
// result through, the outcome is queued back into the parent session as a
// follow-up user message instead -- the same mechanism finishWorkflow uses
// for background workflows completing.
func (c *coordinator) completeSubAgentBackgrounded(params subAgentParams, subSession session.Session, model Model, out subAgentRunOutcome) {
	c.notifyIfUnauthorized(model, out.err)

	var prompt string
	if out.err != nil {
		c.subAgents.finish(subSession.ID, SubAgentFailed, out.err.Error())
		prompt = fmt.Sprintf(
			"The backgrounded sub-agent (session %s) failed: %s\n\nIts progress up to this point is preserved. Retry the agent tool with resume_session_id=%q to continue from where it left off.",
			subSession.ID, out.err, subSession.ID,
		)
	} else if output := subAgentOutput(out.result); subAgentToolCallCount(out.result) == 0 && looksLikeNarratedToolCalls(output) {
		// Degenerate turn: the sub-agent made no tool calls yet emitted
		// tool-call syntax as text, so it did no real work. Flag it and
		// ask for a retry instead of queueing fabricated output as a
		// completion (mirrors completeSubAgentSync).
		c.subAgents.finish(subSession.ID, SubAgentFailed, "sub-agent narrated tool calls as text")
		prompt = fmt.Sprintf(
			"The backgrounded sub-agent (session %s) made no tool calls and emitted tool-call syntax as plain text, so it did no real work. Retry the agent tool with resume_session_id=%q to continue in the same session.",
			subSession.ID, subSession.ID,
		)
	} else {
		// Best-effort, mirroring completeSubAgentSync: a cost-tracking
		// failure must not discard the sub-agent's output.
		c.finalizeSubAgentCost(context.Background(), subSession.ID, params.SessionID)
		c.subAgents.finish(subSession.ID, SubAgentDone, "")
		if output == "" {
			output = "(no text output)"
		}
		prompt = fmt.Sprintf("The backgrounded sub-agent (session %s) finished:\n\n%s", subSession.ID, output)
	}
	c.lingerSubAgentRemoval(subSession.ID)

	// Queue the completion back into the coder session. Coordinator.Run
	// enqueues behind any active turn or pending user messages, so this
	// never interrupts the user -- it fires when the session is idle,
	// exactly like a typed follow-up (see finishWorkflow for the same
	// pattern with background workflows).
	go func() {
		if _, runErr := c.Run(context.Background(), params.SessionID, prompt); runErr != nil {
			slog.Warn("Failed to queue backgrounded sub-agent completion", "session", params.SessionID, "error", runErr)
		}
	}()
}

// subAgentLingerAfterFinish is how long a finished sub-agent remains
// in the registry (and thus AgentList) before being cleared,
// mirroring workflowLingerAfterFinish.
const subAgentLingerAfterFinish = 5 * time.Second

// resumeAgentToolSession validates and loads an existing agent-tool
// session for a resumed "agent" tool call. The session must be a child
// of parentSessionID (the current coder session), so a resume request
// cannot reach into an unrelated conversation, and must not currently
// be busy on another task-agent instance, so two resumes of the same
// session cannot race each other.
func (c *coordinator) resumeAgentToolSession(ctx context.Context, resumeSessionID, parentSessionID string) (session.Session, error) {
	sess, err := c.sessions.Get(ctx, resumeSessionID)
	if err != nil {
		return session.Session{}, fmt.Errorf("resume_session_id %q not found: %w", resumeSessionID, err)
	}
	if sess.ParentSessionID != parentSessionID {
		return session.Session{}, fmt.Errorf("resume_session_id %q does not belong to this conversation", resumeSessionID)
	}
	for taskAgent := range c.taskAgents.Seq() {
		if taskAgent != nil && taskAgent.IsSessionBusy(resumeSessionID) {
			return session.Session{}, fmt.Errorf("session %q is still running; wait for it to finish before resuming", resumeSessionID)
		}
	}
	return sess, nil
}

func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return result.Response.Content.Text()
}

// notifyIfUnauthorized publishes a reauth-required notification when a
// sub-agent turn's error is still an unauthorized failure after retry, for
// providers that support reauth notifications (currently only hyper). It
// is shared by completeSubAgentSync and completeSubAgentBackgrounded so
// both the synchronous and backgrounded sub-agent completion paths notify
// identically.
func (c *coordinator) notifyIfUnauthorized(model Model, err error) {
	if err != nil && c.isUnauthorized(err) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}
}

// finalizeSubAgentCost updates the parent session's cost with a completed
// sub-agent's cost on a best-effort basis, logging (rather than
// returning) any failure so it never discards the sub-agent output that
// was already produced. Shared by completeSubAgentSync and
// completeSubAgentBackgrounded.
func (c *coordinator) finalizeSubAgentCost(ctx context.Context, childSessionID, parentSessionID string) {
	if err := c.updateParentSessionCost(ctx, childSessionID, parentSessionID); err != nil {
		slog.Warn(
			"Failed to update parent session cost",
			"child_session", childSessionID,
			"parent_session", parentSessionID,
			"error", err,
		)
	}
}

// subAgentToolCallCount returns the total number of tool calls the
// sub-agent made across every step of its run.
func subAgentToolCallCount(result *fantasy.AgentResult) int {
	if result == nil {
		return 0
	}
	n := 0
	for _, step := range result.Steps {
		n += len(step.Content.ToolCalls())
	}
	return n
}

// narratedToolCallMarkers are fragments of the raw tool-call harness
// formats that a model emits as plain text when it "calls" a tool without
// actually issuing a structured tool call.
var narratedToolCallMarkers = []string{
	"<function_calls>",
	"<invoke name=",
	"<parameter name=",
	"<invoke",
}

// looksLikeNarratedToolCalls reports whether text contains raw tool-call
// markup, the signature of a model narrating tool calls as prose instead
// of executing them.
func looksLikeNarratedToolCalls(text string) bool {
	for _, m := range narratedToolCallMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// updateParentSessionCost accumulates the cost from a child session to its parent session.
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	childSession, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	parentSession, err := c.sessions.Get(ctx, parentSessionID)
	if err != nil {
		return fmt.Errorf("get parent session: %w", err)
	}

	parentSession.Cost += childSession.Cost

	if _, err := c.sessions.Save(ctx, parentSession); err != nil {
		return fmt.Errorf("save parent session: %w", err)
	}

	return nil
}

// activateSkillAttachments marks any command-palette skill attachments as
// active for the session so their instructions are re-injected on every
// subsequent turn. Attachments are matched by name against the active
// skill set; the skill's parsed instructions are used (not the raw
// attachment) so only genuine skills are activated.
func (c *coordinator) activateSkillAttachments(sessionID string, attachments []message.Attachment) {
	if c.loadedSkills == nil || len(attachments) == 0 || len(c.activeSkills) == 0 {
		return
	}
	for _, att := range attachments {
		if !att.IsMarkdown() {
			continue
		}
		for _, s := range c.activeSkills {
			if s.Name == att.FileName {
				c.loadedSkills.Add(sessionID, s.Name, s.Instructions)
				slog.Debug("Skill activated from attachment", "component", "skills",
					"skill", s.Name, "session_id", sessionID)
				break
			}
		}
	}
}

// deactivateSkillsFromPrompt turns off active skills when the user asks
// (e.g. "stop <skill-name>", "normal mode"). This is the counterpart to
// activation and runs before the turn so a deactivated skill is not
// re-injected on the same turn.
func (c *coordinator) deactivateSkillsFromPrompt(sessionID, prompt string) {
	if c.loadedSkills == nil {
		return
	}
	active := c.loadedSkills.Names(sessionID)
	if len(active) == 0 {
		return
	}
	for _, name := range skillsToDeactivate(prompt, active) {
		c.loadedSkills.Remove(sessionID, name)
	}
}

// skillsToDeactivate returns which of the currently active skills a user
// prompt asks to turn off. A global phrase ("normal mode", "stop all
// skills") deactivates every active skill; otherwise a per-skill phrase
// like "stop <name>", "disable <name>", "turn off <name>" or "unload
// <name>" deactivates that one.
func skillsToDeactivate(prompt string, active []string) []string {
	p := strings.ToLower(prompt)
	if strings.Contains(p, "normal mode") || strings.Contains(p, "stop all skills") {
		return active
	}
	var off []string
	for _, name := range active {
		n := strings.ToLower(name)
		if strings.Contains(p, "stop "+n) ||
			strings.Contains(p, "disable "+n) ||
			strings.Contains(p, "turn off "+n) ||
			strings.Contains(p, "unload "+n) {
			off = append(off, name)
		}
	}
	return off
}

// activeSkillsInjection returns the active-skill instruction block to
// append to a turn, or "" to skip injection. In the default (hybrid)
// mode the block is delivered on activation and after each summarization
// (via TakeInjection's generation gate); when AlwaysReinjectSkills is
// set it is returned on every turn.
func (c *coordinator) activeSkillsInjection(sessionID string) string {
	block := c.loadedSkills.PromptXML(sessionID)
	if block == "" {
		return ""
	}
	if c.cfg.Config().Options.AlwaysReinjectSkills {
		return block
	}
	if c.loadedSkills.TakeInjection(sessionID) {
		return block
	}
	return ""
}

// discoverSkills is a thin fallback wrapper used only when no
// skills.Manager has been threaded through to the coordinator. All
// production call sites (backend.CreateWorkspace, setupLocalWorkspace)
// run discovery in advance and pass the results via the manager;
// reaching this path means a caller bypassed both. It deliberately does
// NOT publish to the package-level broker — there are no subscribers in
// that case, so doing so would be misleading without delivering the
// snapshot anywhere useful.
func discoverSkills(cfg *config.ConfigStore) (allSkills, activeSkills []*skills.Skill) {
	opts := cfg.Config().Options
	var paths, disabled []string
	if opts != nil {
		paths = opts.SkillsPaths
		disabled = opts.DisabledSkills
	}
	var resolver func(string) (string, error)
	if r := cfg.Resolver(); r != nil {
		resolver = r.ResolveValue
	}
	allSkills, activeSkills, states := skills.DiscoverFromConfig(skills.DiscoveryConfig{
		SkillsPaths:    paths,
		DisabledSkills: disabled,
		Resolver:       resolver,
	})
	logDiscoveryStats(states, paths, allSkills, activeSkills, disabled)
	return allSkills, activeSkills
}

// logTurnSkillUsage emits a per-turn diagnostic line showing which skills
// (if any) were loaded during this turn and which looked relevant based on
// a cheap keyword match against the user prompt. The goal is to surface
// "should-have-loaded but didn't" situations for later analysis.
//
// Logged at Info level under component=skills; heavy fields are elided when
// there is nothing interesting to report.
func logTurnSkillUsage(
	sessionID string,
	prompt string,
	activeSkills []*skills.Skill,
	tracker *skills.Tracker,
	before []string,
) {
	if tracker == nil || len(activeSkills) == 0 {
		return
	}

	after := tracker.LoadedNames()

	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	var loadedThisTurn []string
	for _, n := range after {
		if !beforeSet[n] {
			loadedThisTurn = append(loadedThisTurn, n)
		}
	}

	slog.Info(
		"Skill turn summary",
		"component", "skills",
		"session_id", sessionID,
		"prompt_len", len(prompt),
		"active_total", len(activeSkills),
		"loaded_total", len(after),
		"loaded_this_turn", loadedThisTurn,
	)
}

// logDiscoveryStats emits a single structured log line summarising skill
// discovery for the current session. It is intentionally low-volume: one
// line per session start. Builtin vs user counts are derived from the
// SkillState.Path — builtin states use the "builtin/" embed prefix.
func logDiscoveryStats(
	states []*skills.SkillState,
	userPaths []string,
	allSkills, activeSkills []*skills.Skill,
	disabled []string,
) {
	var builtinOK, builtinErr, userOK, userErr int
	for _, s := range states {
		isBuiltin := strings.HasPrefix(s.Path, "builtin/")
		switch {
		case isBuiltin && s.State == skills.StateNormal:
			builtinOK++
		case isBuiltin && s.State == skills.StateError:
			builtinErr++
		case !isBuiltin && s.State == skills.StateNormal:
			userOK++
		case !isBuiltin && s.State == skills.StateError:
			userErr++
		}
	}

	activeNames := make([]string, 0, len(activeSkills))
	for _, s := range activeSkills {
		activeNames = append(activeNames, s.Name)
	}

	xml := skills.ToPromptXML(activeSkills)

	slog.Info(
		"Skill discovery complete",
		"component", "skills",
		"builtin_ok", builtinOK,
		"builtin_errors", builtinErr,
		"user_ok", userOK,
		"user_errors", userErr,
		"user_paths", len(userPaths),
		"deduped_total", len(allSkills),
		"active", len(activeSkills),
		"disabled", len(disabled),
		"prompt_bytes", len(xml),
		"prompt_tok_est", skills.ApproxTokenCount(xml),
		"active_names", activeNames,
	)
}
