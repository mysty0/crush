// Package workspace defines the Workspace interface used by all
// frontends (TUI, CLI) to interact with a running workspace. Two
// implementations exist: one wrapping a local app.App instance and one
// wrapping the HTTP client SDK.
package workspace

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent"
	mcptools "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/rewind"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
)

// LSPClientInfo holds information about an LSP client's state. This is
// the frontend-facing type; implementations translate from the
// underlying app or proto representation.
type LSPClientInfo struct {
	Name            string
	State           lsp.ServerState
	Error           error
	DiagnosticCount int
	ConnectedAt     time.Time
}

// LSPEventType represents the type of LSP event.
type LSPEventType string

const (
	LSPEventStateChanged       LSPEventType = "state_changed"
	LSPEventDiagnosticsChanged LSPEventType = "diagnostics_changed"
)

// LSPEvent represents an LSP event forwarded to the TUI.
type LSPEvent struct {
	Type            LSPEventType
	Name            string
	State           lsp.ServerState
	Error           error
	DiagnosticCount int
}

// AgentModel holds the model information exposed to the UI.
type AgentModel struct {
	CatwalkCfg catwalk.Model
	ModelCfg   config.SelectedModel
}

// Workspace is the main abstraction consumed by the TUI and CLI. It
// groups every operation a frontend needs to perform against a running
// workspace, regardless of whether the workspace is in-process or
// remote.
type Workspace interface {
	// Sessions
	CreateSession(ctx context.Context, title string) (session.Session, error)
	GetSession(ctx context.Context, sessionID string) (session.Session, error)
	ListSessions(ctx context.Context) ([]session.Session, error)
	SaveSession(ctx context.Context, sess session.Session) (session.Session, error)
	DeleteSession(ctx context.Context, sessionID string) error
	CreateAgentToolSessionID(messageID, toolCallID string) string
	ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool)
	// SetCurrentSession reports the session this client is currently
	// viewing. Empty sessionID clears the entry (e.g. landing screen).
	// In single-client local mode this is a no-op. In client/server
	// mode it informs the server's per-client presence map so other
	// observers can compute attached-client counts per session.
	SetCurrentSession(ctx context.Context, sessionID string) error

	// Messages
	ListMessages(ctx context.Context, sessionID string) ([]message.Message, error)
	ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error)
	ListAllUserMessages(ctx context.Context) ([]message.Message, error)
	// DiscardMessages permanently deletes the given messages from a
	// session. Used to drop a canceled no-output turn so its prompt can be
	// returned to the editor.
	DiscardMessages(ctx context.Context, sessionID string, messageIDs ...string) error

	// Agent
	AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error
	AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error)
	AgentCancel(sessionID string)
	AgentIsBusy() bool
	AgentIsSessionBusy(sessionID string) bool
	AgentModel() AgentModel
	AgentIsReady() bool
	AgentQueuedPrompts(sessionID string) int
	AgentQueuedPromptsList(sessionID string) []string
	AgentClearQueue(sessionID string)
	AgentSummarize(ctx context.Context, sessionID string) error
	// AgentSendToSubAgent steers a currently-running sub-agent turn by
	// injecting a follow-up user prompt into its session. It errors if
	// the sub-agent session is not currently busy (e.g. it already
	// finished): follow-ups are only supported mid-turn.
	AgentSendToSubAgent(ctx context.Context, subAgentSessionID, prompt string) error
	// AgentCancelSubAgent cancels a currently-running sub-agent turn.
	// It is a no-op if the sub-agent session is not currently busy.
	AgentCancelSubAgent(subAgentSessionID string)

	// AgentRunningWorkflows returns a snapshot of every background
	// workflow (dispatched via the "Workflow" tool), running or
	// recently finished but not yet cleared.
	AgentRunningWorkflows() []agent.WorkflowStatus
	// AgentWorkflowStatus returns the current status of a workflow by
	// its (workflow) session ID.
	AgentWorkflowStatus(workflowSessionID string) (agent.WorkflowStatus, bool)
	// AgentCancelWorkflow cancels a running background workflow by its
	// (workflow) session ID.
	AgentCancelWorkflow(workflowSessionID string)
	// AgentRunningSchedules returns a snapshot of every scheduled task
	// (dispatched via ScheduleCron/ScheduleWakeup), active or recently
	// stopped.
	AgentRunningSchedules() []agent.ScheduledTaskStatus
	// AgentCancelSchedule stops a scheduled task by its task ID. It is
	// a no-op if the task is unknown or already stopped.
	AgentCancelSchedule(taskID string)

	// RewindListPoints returns the user messages a session can be rewound
	// to, newest first.
	RewindListPoints(ctx context.Context, sessionID string) ([]rewind.RewindPoint, error)
	// Rewind rewinds a session to a message using the given mode, forking
	// the conversation and/or restoring files on disk. It returns the new
	// forked session ID (empty for code-only rewinds) and how many files
	// were restored.
	Rewind(ctx context.Context, sessionID, messageID string, mode rewind.Mode) (rewind.Result, error)
	// AgentRegenerateTitle re-runs AI title generation for a session using
	// its first user message.
	AgentRegenerateTitle(ctx context.Context, sessionID string) error
	UpdateAgentModel(ctx context.Context) error
	InitCoderAgent(ctx context.Context) error
	GetDefaultSmallModel(providerID string) config.SelectedModel

	// Permissions
	//
	// PermissionGrant, PermissionGrantPersistent, and PermissionDeny
	// return true if the call resolved the pending request and false if
	// it had already been resolved by another subscriber (or is no
	// longer pending). A false return is not an error; the modal can
	// still close locally because the resolution will arrive via the
	// PermissionNotification event stream regardless of which client
	// won the race.
	PermissionGrant(perm permission.PermissionRequest) bool
	PermissionGrantPersistent(perm permission.PermissionRequest) bool
	PermissionDeny(perm permission.PermissionRequest) bool
	PermissionSkipRequests() bool
	PermissionSetSkipRequests(skip bool)
	// PermissionPlanMode reports whether plan mode is active. In plan mode
	// mutating tools are blocked so the agent can only research and propose a
	// plan.
	PermissionPlanMode() bool
	PermissionSetPlanMode(plan bool)

	// FileTracker
	FileTrackerRecordRead(ctx context.Context, sessionID, path string)
	FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time
	FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error)

	// History
	ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error)

	// LSP
	LSPStart(ctx context.Context, path string)
	LSPStopAll(ctx context.Context)
	LSPGetStates() map[string]LSPClientInfo
	LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts

	// Config (read-only data)
	Config() *config.Config
	WorkingDir() string
	Resolver() config.VariableResolver

	// Config mutations (proxied to server in client mode)
	UpdatePreferredModel(scope config.Scope, modelType config.SelectedModelType, model config.SelectedModel) error
	SetCompactMode(scope config.Scope, enabled bool) error
	SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error
	SetConfigField(scope config.Scope, key string, value any) error
	RemoveConfigField(scope config.Scope, key string) error
	ImportCopilot() (*oauth.Token, bool)
	RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error

	// Project lifecycle
	ProjectNeedsInitialization() (bool, error)
	MarkProjectInitialized() error
	InitializePrompt() (string, error)
	ListSkills(ctx context.Context) ([]skills.CatalogEntry, error)
	ReadSkill(ctx context.Context, skillID string) ([]byte, skills.SkillReadResult, error)
	// RefreshSkills re-runs skill discovery so newly added or removed
	// skill files are reflected without restarting. In client/server mode
	// discovery is owned by the server and this is a no-op.
	RefreshSkills(ctx context.Context)

	// MCP operations (server-side in client mode)
	MCPGetStates() map[string]mcptools.ClientInfo
	MCPRefreshPrompts(ctx context.Context, name string)
	MCPRefreshResources(ctx context.Context, name string)
	RefreshMCPTools(ctx context.Context, name string)
	ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error)
	GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error)
	EnableDockerMCP(ctx context.Context) error
	DisableDockerMCP() error

	// Events
	Subscribe(program *tea.Program)
	Shutdown()
}

// MCPResourceContents holds the contents of an MCP resource.
type MCPResourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mime_type,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     []byte `json:"blob,omitempty"`
}
