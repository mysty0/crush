package agent

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/workflow"
)

//go:embed templates/workflow_tool.md.tpl
var workflowToolDescriptionTmpl string

//go:embed templates/workflow_subagent.md.tpl
var workflowSubAgentPromptTmpl []byte

// WorkflowToolName is the name of the Workflow tool.
const WorkflowToolName = "Workflow"

// WorkflowParams are the parameters for the Workflow tool.
type WorkflowParams struct {
	Name  string `json:"name" description:"The workflow to run, e.g. 'deep-research'"`
	Args  string `json:"args,omitempty" description:"Freeform argument passed to the workflow, e.g. the research question"`
	Model string `json:"model,omitempty" description:"Optional. The ID of the model to run this workflow's sub-agents on, chosen from the list of available models in this tool's description. Omit to use the default model. Prefer a more capable model for hard, reasoning-heavy research and a smaller, faster model for cheaper, simpler runs."`
}

// workflowSubAgentTools returns the fixed, read-only tool policy every
// workflow-dispatched agent() call runs under, regardless of which
// phase of the script issued it: web search/fetch plus local
// code-search and file-reading, with no ability to edit files, run
// shell commands, or spawn further agents/workflows. Phase-specific
// behavior (search vs. extract vs. verify) comes entirely from the
// prompt text the script sends, matching how the reference workflow
// this was ported from scopes its sub-agent.
func (c *coordinator) workflowSubAgentTools(tmpDir string, client *http.Client) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		tools.NewWebSearchTool(client, c.webSearchOptions()),
		tools.NewWebFetchTool(tmpDir, client, c.webFetchOptions()),
		tools.NewGlobTool(tmpDir, c.cfg.Config().Tools.Glob),
		tools.NewGrepTool(tmpDir, c.cfg.Config().Tools.Grep),
		tools.NewSourcegraphTool(client),
		// Sub-agent read: hashline mode off (empty mode + nil store).
		tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, nil, nil, "", nil, tmpDir),
	}
}

// workflowToolDescription renders the Workflow tool's description,
// listing every discovered workflow's name/description/whenToUse so
// the model knows what's available without a hardcoded prompt.
func workflowToolDescription(workflows []*workflow.Workflow) (string, error) {
	tmpl, err := template.New("workflow_tool").Parse(workflowToolDescriptionTmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Workflows []*workflow.Workflow }{workflows}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (c *coordinator) workflowTool(ctx context.Context, client *http.Client) (fantasy.AgentTool, error) {
	workflows, err := workflow.Discover()
	if err != nil {
		return nil, fmt.Errorf("discover workflows: %w", err)
	}
	description, err := workflowToolDescription(workflows)
	if err != nil {
		return nil, fmt.Errorf("render workflow tool description: %w", err)
	}
	description += c.availableModelsDescription()

	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 10
		transport.IdleConnTimeout = 90 * time.Second

		client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	}

	return fantasy.NewParallelAgentTool(
		WorkflowToolName,
		description,
		func(ctx context.Context, params WorkflowParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Name == "" {
				return fantasy.NewTextErrorResponse("name is required"), nil
			}

			var wf *workflow.Workflow
			for _, w := range workflows {
				if w.Name == params.Name {
					wf = w
					break
				}
			}
			if wf == nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Unknown workflow: %q", params.Name)), nil
			}

			if params.Model != "" {
				if _, ok := c.resolveTaskModel(params.Model); !ok {
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"unknown model %q; choose one of the available model IDs: %s",
						params.Model, strings.Join(c.availableModelIDs(), ", "),
					)), nil
				}
			}

			coderSessionID := tools.GetSessionFromContext(ctx)
			if coderSessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			p, err := c.permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   coderSessionID,
					Path:        c.cfg.WorkingDir(),
					ToolCallID:  call.ID,
					ToolName:    WorkflowToolName,
					Action:      "run",
					Description: fmt.Sprintf("Run workflow: %s", params.Name),
					Params:      params,
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return tools.NewPermissionDeniedResponse(), nil
			}

			// Create the dedicated workflow session up front, parented
			// to the coder session. It is the cancel/view handle and the
			// parent of every sub-agent the workflow spawns, so the
			// two-pane view can discover them by ParentSessionID.
			workflowSessionID := c.sessions.CreateAgentToolSessionID(agentMessageID, call.ID)
			if _, err := c.sessions.CreateTaskSession(ctx, workflowSessionID, coderSessionID, "Workflow: "+params.Name); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("create workflow session: %w", err)
			}

			// The workflow runs in the background under its own context,
			// independent of the coder turn: launching it must not block
			// the turn, and a normal turn-end must not cancel it. The
			// context is cancelable via the registry (CancelWorkflow)
			// and bounded by the engine's own timeout.
			bgCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

			c.workflows.register(WorkflowStatus{
				SessionID:       workflowSessionID,
				ToolCallID:      call.ID,
				ParentSessionID: coderSessionID,
				Name:            params.Name,
				Args:            params.Args,
				State:           WorkflowRunning,
				StartedAt:       time.Now(),
			}, cancel)

			go c.runWorkflowBackground(bgCtx, cancel, workflowBackgroundParams{
				workflow:          wf,
				args:              params.Args,
				coderSessionID:    coderSessionID,
				workflowSessionID: workflowSessionID,
				toolCallID:        call.ID,
				agentMessageID:    agentMessageID,
				httpClient:        client,
				modelID:           params.Model,
			})

			return fantasy.NewTextResponse(fmt.Sprintf(
				"Started the %s workflow in the background (session %s). It runs while we keep working; I'll report back with the results when it finishes.",
				params.Name, workflowSessionID,
			)), nil
		},
	), nil
}

// workflowBackgroundParams carries everything runWorkflowBackground
// needs to execute one workflow run.
type workflowBackgroundParams struct {
	workflow          *workflow.Workflow
	args              string
	coderSessionID    string
	workflowSessionID string
	toolCallID        string
	agentMessageID    string
	httpClient        *http.Client
	// modelID optionally overrides the workflow's default (large)
	// model, validated against resolveTaskModel before the background
	// run starts. Empty uses the configured large model.
	modelID string
}

// resolveWorkflowModels resolves the primary/small model pair a
// workflow run uses. modelID overrides the primary (large) model when
// non-empty (already validated by the tool handler); the small model
// is always the configured one (used for CoerceObject and any
// "small"-tier per-call override).
func (c *coordinator) resolveWorkflowModels(ctx context.Context, modelID string) (Model, Model, error) {
	large, small, err := c.buildAgentModels(ctx, true)
	if err != nil {
		return Model{}, Model{}, err
	}
	if modelID == "" {
		return large, small, nil
	}
	selected, ok := c.resolveTaskModel(modelID)
	if !ok {
		// Already validated in the tool handler; defensive fallback.
		return large, small, nil
	}
	primary, err := c.buildModelFromSelected(ctx, selected, true)
	if err != nil {
		return Model{}, Model{}, err
	}
	return primary, small, nil
}

// buildWorkflowSubAgent builds a SessionAgent that runs a workflow's
// agent() calls on the given primary model, under the workflow's
// fixed, restricted tool policy (workflowSubAgentTools). Used for the
// run's default model and lazily by workflowRunner for any per-call
// model override (see AgentRequest.Model).
func (c *coordinator) buildWorkflowSubAgent(ctx context.Context, tmpDir string, primary, small Model, httpClient *http.Client) (SessionAgent, error) {
	providerCfg, ok := c.cfg.Config().Providers.Get(primary.ModelCfg.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q not configured", primary.ModelCfg.Provider)
	}
	promptTemplate, err := prompt.NewPrompt("workflow-subagent", string(workflowSubAgentPromptTmpl), prompt.WithWorkingDir(tmpDir))
	if err != nil {
		return nil, err
	}
	systemPrompt, err := promptTemplate.Build(ctx, primary.Model.Provider(), primary.Model.Model(), c.cfg)
	if err != nil {
		return nil, err
	}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:           primary,
		SmallModel:           small,
		SystemPromptPrefix:   providerCfg.SystemPromptPrefix,
		SystemPrompt:         systemPrompt,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                c.workflowSubAgentTools(tmpDir, httpClient),
	}), nil
}

// workflowModelCacheKey identifies a built workflow sub-agent by the
// model it runs on, for workflowRunner's per-model agent cache.
func workflowModelCacheKey(m Model) string {
	return m.ModelCfg.Provider + "/" + m.ModelCfg.Model
}

// runWorkflowBackground executes a workflow to completion on a
// background goroutine, updating the registry as phases advance and
// agents dispatch, then queues a completion summary back into the coder
// session (via the coordinator's normal Run dispatch, which lands in
// the session's message queue if it is busy — exactly like a typed
// message).
func (c *coordinator) runWorkflowBackground(ctx context.Context, cancel context.CancelFunc, p workflowBackgroundParams) {
	defer cancel()

	tmpDir, err := os.MkdirTemp(c.cfg.Config().Options.DataDirectory, "crush-workflow-*")
	if err != nil {
		c.finishWorkflow(p, WorkflowFailed, fmt.Sprintf("Failed to create workspace: %s", err), nil)
		return
	}
	defer os.RemoveAll(tmpDir)

	large, small, err := c.resolveWorkflowModels(ctx, p.modelID)
	if err != nil {
		c.finishWorkflow(p, WorkflowFailed, fmt.Sprintf("Failed to build models: %s", err), nil)
		return
	}

	subAgent, err := c.buildWorkflowSubAgent(ctx, tmpDir, large, small, p.httpClient)
	if err != nil {
		c.finishWorkflow(p, WorkflowFailed, fmt.Sprintf("Failed to build agent: %s", err), nil)
		return
	}

	runner := &workflowRunner{
		c:                 c,
		tmpDir:            tmpDir,
		httpClient:        p.httpClient,
		smallModel:        small,
		workflowSessionID: p.workflowSessionID,
		agents:            map[string]SessionAgent{workflowModelCacheKey(large): subAgent},
		defaultAgent:      subAgent,
		defaultModel:      large,
	}

	// Stream a human-readable transcript into the in-place tool call so
	// the user can watch progress inline, and mirror phase transitions
	// into the registry that drives the two-pane view.
	var (
		progressMu  sync.Mutex
		progressBuf strings.Builder
	)
	emit := func(line string) {
		progressMu.Lock()
		progressBuf.WriteString(line)
		progressBuf.WriteByte('\n')
		out := progressBuf.String()
		progressMu.Unlock()
		tools.PublishWorkflowProgress(p.coderSessionID, p.toolCallID, out)
	}

	result, err := workflow.Run(ctx, workflow.RunOptions{
		Script: p.workflow.Script,
		Args:   p.args,
		Runner: runner,
		Progress: func(e workflow.ProgressEvent) {
			switch {
			case e.Phase != "":
				c.workflows.setPhase(p.workflowSessionID, e.Phase)
				emit("### " + e.Phase)
			case e.Log != "":
				emit("  " + e.Log)
			}
			c.publishWorkflowStatus(p.workflowSessionID)
		},
	})

	switch {
	case errors.Is(err, context.Canceled):
		c.finishWorkflow(p, WorkflowCanceled, "The workflow was canceled.", nil)
	case err != nil:
		c.finishWorkflow(p, WorkflowFailed, fmt.Sprintf("The workflow failed: %s", err), nil)
	default:
		c.finishWorkflow(p, WorkflowCompleted, "", result)
	}
}

// finishWorkflow records a workflow's terminal state, writes its full
// report to disk (on success), streams a final progress line, and
// queues a completion summary back into the coder session so the main
// agent can react. It then removes the workflow from the registry.
func (c *coordinator) finishWorkflow(p workflowBackgroundParams, state WorkflowRunState, message string, result any) {
	var reportPath string
	summary := message

	if state == WorkflowCompleted && result != nil {
		data, err := json.MarshalIndent(result, "", "  ")
		if err == nil {
			reportPath = filepath.Join(
				c.cfg.Config().Options.DataDirectory,
				fmt.Sprintf("workflow-%s-report.json", sanitizeFileToken(p.workflowSessionID)),
			)
			if writeErr := os.WriteFile(reportPath, data, 0o644); writeErr != nil {
				slog.Warn("Failed to write workflow report", "path", reportPath, "error", writeErr)
				reportPath = ""
			}
		}
		summary = summarizeWorkflowResult(result)
	}

	c.workflows.finish(p.workflowSessionID, state, summary, reportPath)
	c.publishWorkflowStatus(p.workflowSessionID)

	// Build the message queued back into the coder session. On success
	// it references the on-disk report so the agent can Read the full
	// data; on failure/cancel it just reports the outcome.
	var prompt string
	switch state {
	case WorkflowCompleted:
		prompt = fmt.Sprintf(
			"The %s workflow finished.\n\n%s\n\nThe full structured report was saved to %s — Read that file if you need the complete findings, sources, or per-claim detail. You can also inspect the workflow's sub-agents with `crush session show %s --json`.",
			p.workflow.Name, summary, reportPath, p.workflowSessionID,
		)
	case WorkflowCanceled:
		prompt = fmt.Sprintf("The %s workflow was canceled before it finished.", p.workflow.Name)
	default:
		prompt = fmt.Sprintf("The %s workflow failed: %s", p.workflow.Name, summary)
	}

	// Queue the completion back into the coder session. Coordinator.Run
	// enqueues behind any active turn or pending user messages, so this
	// never interrupts the user — it fires when the session is idle,
	// exactly like a typed follow-up.
	go func() {
		if _, err := c.Run(context.Background(), p.coderSessionID, prompt); err != nil {
			slog.Warn("Failed to queue workflow completion", "session", p.coderSessionID, "error", err)
		}
	}()

	// Keep the finished status visible briefly so the UI can render the
	// terminal state, then drop it from the running list.
	go func() {
		time.Sleep(workflowLingerAfterFinish)
		c.workflows.remove(p.workflowSessionID)
		c.publishWorkflowStatus(p.workflowSessionID)
	}()
}

// workflowLingerAfterFinish is how long a finished workflow remains in
// the registry (and thus the running list / view) before being cleared.
const workflowLingerAfterFinish = 5 * time.Second

// publishWorkflowStatus notifies subscribers that a workflow's status
// changed so the UI can refresh the two-pane view and the picker list.
func (c *coordinator) publishWorkflowStatus(workflowSessionID string) {
	status, ok := c.workflows.get(workflowSessionID)
	if !ok {
		return
	}
	tools.PublishWorkflowStatus(status.SessionID, status.ToolCallID)
}

// RunningWorkflows implements Coordinator.
func (c *coordinator) RunningWorkflows() []WorkflowStatus {
	return c.workflows.list()
}

// WorkflowStatus implements Coordinator.
func (c *coordinator) WorkflowStatus(workflowSessionID string) (WorkflowStatus, bool) {
	return c.workflows.get(workflowSessionID)
}

// CancelWorkflow implements Coordinator.
func (c *coordinator) CancelWorkflow(workflowSessionID string) {
	c.workflows.cancel(workflowSessionID)
}

// workflowRunner implements workflow.Runner by dispatching each
// agent() call as a real sub-agent turn parented under the workflow
// session (own child session, cost tracking, full transcript), and
// coercing structured output with the small model. It records each
// dispatched agent in the coordinator's workflow registry so the
// two-pane view can show per-agent stats.
type workflowRunner struct {
	c                 *coordinator
	workflowSessionID string
	tmpDir            string
	httpClient        *http.Client
	smallModel        Model
	defaultAgent      SessionAgent
	defaultModel      Model

	// agents caches SessionAgents built for a per-call model override
	// (see AgentRequest.Model), keyed by workflowModelCacheKey. Guarded
	// by agentsMu since agent() calls can run concurrently up to the
	// run's MaxConcurrency.
	agentsMu sync.Mutex
	agents   map[string]SessionAgent
}

// agentFor resolves a per-call model request to a SessionAgent and the
// Model it runs on: "" uses the workflow's default model, "small" uses
// the run's configured small/fast model, and any other value is an
// explicit model ID. Agents for non-default models are built lazily
// and cached for the rest of the run.
func (r *workflowRunner) agentFor(ctx context.Context, modelID string) (SessionAgent, Model, error) {
	var target Model
	switch modelID {
	case "":
		return r.defaultAgent, r.defaultModel, nil
	case "small":
		target = r.smallModel
	default:
		selected, ok := r.c.resolveTaskModel(modelID)
		if !ok {
			return nil, Model{}, fmt.Errorf(
				"unknown model %q; choose one of the available model IDs: %s",
				modelID, strings.Join(r.c.availableModelIDs(), ", "),
			)
		}
		built, err := r.c.buildModelFromSelected(ctx, selected, true)
		if err != nil {
			return nil, Model{}, err
		}
		target = built
	}

	key := workflowModelCacheKey(target)
	r.agentsMu.Lock()
	defer r.agentsMu.Unlock()
	if existing, ok := r.agents[key]; ok {
		return existing, target, nil
	}
	built, err := r.c.buildWorkflowSubAgent(ctx, r.tmpDir, target, r.smallModel, r.httpClient)
	if err != nil {
		return nil, Model{}, err
	}
	r.agents[key] = built
	return built, target, nil
}

func (r *workflowRunner) RunAgent(ctx context.Context, req workflow.AgentRequest) (string, error) {
	// Each agent call gets a unique child session parented under the
	// workflow session, so the view can group them by phase and pull
	// per-agent stats.
	agentSessionID := r.c.sessions.CreateAgentToolSessionID(r.workflowSessionID, fmt.Sprintf("agent-%d", req.Seq))

	selectedAgent, model, err := r.agentFor(ctx, req.Model)
	if err != nil {
		return "", err
	}

	r.c.workflows.recordAgent(r.workflowSessionID, WorkflowAgentStatus{
		SessionID: agentSessionID,
		Label:     req.Label,
		Phase:     req.Phase,
		Provider:  model.ModelCfg.Provider,
		Model:     model.ModelCfg.Model,
		StartedAt: time.Now(),
	})

	resp, err := r.c.runSubAgent(ctx, subAgentParams{
		Agent:          selectedAgent,
		SessionID:      r.workflowSessionID,
		AgentMessageID: r.workflowSessionID,
		ToolCallID:     fmt.Sprintf("agent-%d", req.Seq),
		Prompt:         req.Prompt,
		SessionTitle:   cmp.Or(req.Label, "Workflow Agent"),
		ToolName:       WorkflowToolName,
		Label:          req.Label,
	})

	r.c.workflows.markAgentDone(r.workflowSessionID, agentSessionID)

	if err != nil {
		return "", err
	}
	if resp.IsError {
		return "", errors.New(resp.Content)
	}
	return resp.Content, nil
}

// schemaNameSanitizer replaces every character disallowed by
// Anthropic's tool-name pattern (^[a-zA-Z0-9_-]{1,128}$) with an
// underscore. Workflow labels like "search:Recent news" or
// "search:Official/authoritative" are used as schema names for
// structured-output coercion, and those contain colons, spaces, and
// slashes that the API rejects.
var schemaNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeSchemaName(name string) string {
	name = schemaNameSanitizer.ReplaceAllString(name, "_")
	if name == "" {
		name = "result"
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}

// sanitizeFileToken makes a string safe to embed in a filename by
// replacing every character outside [a-zA-Z0-9_-] with an underscore.
func sanitizeFileToken(s string) string {
	return schemaNameSanitizer.ReplaceAllString(s, "_")
}

// summarizeWorkflowResult produces a short human-readable summary of a
// workflow's structured result for the completion message queued back
// into the coder session. It prefers a top-level "summary" string when
// present (as deep-research reports include), falling back to a compact
// note when absent.
func summarizeWorkflowResult(result any) string {
	if m, ok := result.(map[string]any); ok {
		if s, ok := m["summary"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		if e, ok := m["error"].(string); ok && strings.TrimSpace(e) != "" {
			return strings.TrimSpace(e)
		}
	}
	return "The workflow completed. See the full report for details."
}

func (r *workflowRunner) CoerceObject(ctx context.Context, text string, schema *workflow.Schema, schemaName string) (any, error) {
	schemaName = sanitizeSchemaName(cmp.Or(schemaName, "result"))
	fantasySchema := workflowSchemaToFantasy(schema)
	resp, err := r.smallModel.Model.GenerateObject(ctx, fantasy.ObjectCall{
		Prompt: fantasy.Prompt{
			fantasy.NewUserMessage("Extract the following into the requested structure:\n\n" + text),
		},
		Schema:     fantasySchema,
		SchemaName: schemaName,
		RepairText: r.repairObjectText(schemaName, fantasySchema),
	})
	if err != nil {
		return nil, err
	}
	return resp.Object, nil
}

// repairObjectText builds a RepairText callback for GenerateObject: when
// the model's structured output fails schema validation, it logs the raw
// text that failed (otherwise unrecoverable once discarded) and asks the
// small model once to fix it, quoting the exact validation error back so
// the retry can address it directly.
func (r *workflowRunner) repairObjectText(schemaName string, schema fantasy.Schema) func(ctx context.Context, text string, verr error) (string, error) {
	return func(ctx context.Context, text string, verr error) (string, error) {
		slog.Warn("Workflow structured output failed validation, attempting repair",
			"schema", schemaName, "error", verr, "raw_text", text)

		schemaJSON, err := json.Marshal(schema)
		if err != nil {
			return "", err
		}

		resp, err := r.smallModel.Model.Generate(ctx, fantasy.Call{
			Prompt: fantasy.Prompt{
				fantasy.NewUserMessage(fmt.Sprintf(
					"The following JSON failed schema validation.\n\n"+
						"## Schema\n%s\n\n## Invalid JSON\n%s\n\n## Validation error\n%s\n\n"+
						"Return only the corrected JSON, with no surrounding text.",
					schemaJSON, text, verr,
				)),
			},
		})
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(resp.Content.Text()), nil
	}
}

// workflowSchemaToFantasy converts the workflow engine's
// dependency-free Schema type into fantasy.Schema for structured
// output generation.
func workflowSchemaToFantasy(s *workflow.Schema) fantasy.Schema {
	if s == nil {
		return fantasy.Schema{}
	}
	out := fantasy.Schema{
		Type:        s.Type,
		Description: s.Description,
		Required:    s.Required,
		Format:      s.Format,
		Enum:        s.Enum,
		Minimum:     s.Minimum,
		Maximum:     s.Maximum,
		MinLength:   s.MinLength,
		MaxLength:   s.MaxLength,
	}
	if s.Items != nil {
		items := workflowSchemaToFantasy(s.Items)
		out.Items = &items
	}
	if s.Properties != nil {
		out.Properties = make(map[string]*fantasy.Schema, len(s.Properties))
		for k, v := range s.Properties {
			converted := workflowSchemaToFantasy(v)
			out.Properties[k] = &converted
		}
	}
	return out
}
