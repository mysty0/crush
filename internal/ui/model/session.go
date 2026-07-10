package model

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/rewind"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/tmux"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/x/ansi"
)

// loadSessionMsg is a message indicating that a session and its files have
// been loaded.
type loadSessionMsg struct {
	session   *session.Session
	files     []SessionFile
	readFiles []string
}

// rewindDoneMsg is emitted after a rewind completes. For fork modes it
// carries the new session to switch to.
type rewindDoneMsg struct {
	result rewind.Result
	mode   rewind.Mode
}

// rewindFilesPlural returns the plural suffix for a file count.
func rewindFilesPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// lspFilePaths returns deduplicated file paths from both modified and read
// files for starting LSP servers.
func (msg loadSessionMsg) lspFilePaths() []string {
	seen := make(map[string]struct{}, len(msg.files)+len(msg.readFiles))
	paths := make([]string, 0, len(msg.files)+len(msg.readFiles))
	for _, f := range msg.files {
		p := f.LatestVersion.Path
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for _, p := range msg.readFiles {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	return paths
}

// SessionFile tracks the first and latest versions of a file in a session,
// along with the total additions and deletions.
type SessionFile struct {
	FirstVersion  history.File
	LatestVersion history.File
	Additions     int
	Deletions     int
}

// loadSession loads the session along with its associated files and computes
// the diff statistics (additions and deletions) for each file in the session.
// It returns a tea.Cmd that, when executed, fetches the session data and
// returns a sessionFilesLoadedMsg containing the processed session files.
//
// The returned batch also reports the new current-session selection to
// the workspace so the server can update its per-client presence map.
// That report is fire-and-forget: errors are logged at debug and the
// UI never blocks on the call.
func (m *UI) loadSession(sessionID string) tea.Cmd {
	load := func() tea.Msg {
		session, err := m.com.Workspace.GetSession(context.Background(), sessionID)
		if err != nil {
			return util.ReportError(err)
		}

		sessionFiles, err := m.loadSessionFiles(sessionID)
		if err != nil {
			return util.ReportError(err)
		}

		readFiles, err := m.com.Workspace.FileTrackerListReadFiles(context.Background(), sessionID)
		if err != nil {
			slog.Error("Failed to load read files for session", "error", err)
		}

		return loadSessionMsg{
			session:   &session,
			files:     sessionFiles,
			readFiles: readFiles,
		}
	}
	return tea.Batch(load, m.reportCurrentSession(sessionID), m.reconcileStuckSession(sessionID))
}

// lastSessionModel returns the provider/model of the most recent
// assistant message in msgs that recorded one, so a session can be
// reopened using the same model it was last using instead of whatever
// model happens to be globally selected right now. ok is false if no
// assistant message in the session recorded a model (e.g. an empty or
// not-yet-responded-to session).
func lastSessionModel(msgs []message.Message) (provider, model string, ok bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.Model == "" || msg.Provider == "" {
			continue
		}
		return msg.Provider, msg.Model, true
	}
	return "", "", false
}

// restoreSessionModel switches the active large model to whichever
// model most recently produced output in this session, if different
// from the currently active model and still usable — provider
// configured with credentials and model still known to it. This makes
// reopening a session pick up right where it left off instead of
// silently continuing with whatever model happens to be globally
// selected. Best-effort and silent otherwise: a session whose model
// is no longer available (e.g. removed from config) simply keeps
// using the current one, and this never prompts for
// re-authentication on what is otherwise a silent session switch.
func (m *UI) restoreSessionModel(msgs []message.Message) tea.Cmd {
	if !m.com.Workspace.AgentIsReady() {
		return nil
	}
	provider, model, ok := lastSessionModel(msgs)
	if !ok {
		return nil
	}

	current := m.com.Workspace.AgentModel()
	if current.ModelCfg.Provider == provider && current.ModelCfg.Model == model {
		return nil
	}

	cfg := m.com.Config()
	if cfg == nil {
		return nil
	}
	if _, configured := cfg.Providers.Get(provider); !configured {
		return nil
	}
	catwalkModel := cfg.GetModel(provider, model)
	if catwalkModel == nil {
		return nil
	}

	selected := config.SelectedModel{Model: model, Provider: provider}
	if err := m.com.Workspace.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, selected); err != nil {
		return util.ReportError(err)
	}
	m.applyThemeForProvider(provider)

	modelName := cmp.Or(catwalkModel.Name, model)
	return func() tea.Msg {
		if err := m.com.Workspace.UpdateAgentModel(context.Background()); err != nil {
			return util.ReportError(err)
		}
		return util.NewInfoMsg(fmt.Sprintf("Restored session model: %s", modelName))
	}
}

// reportCurrentSession returns a fire-and-forget tea.Cmd that
// informs the workspace which session this client is currently
// viewing. Errors are logged at debug only; the call is a hint
// for server-side presence tracking, not correctness-critical
// state.
func (m *UI) reportCurrentSession(sessionID string) tea.Cmd {
	return func() tea.Msg {
		if err := m.com.Workspace.SetCurrentSession(context.Background(), sessionID); err != nil {
			slog.Debug("Failed to report current session", "session_id", sessionID, "error", err)
		}
		return nil
	}
}

// reconcileStuckSession asynchronously reconciles any tool calls left
// unfinished by an interrupted run (e.g. a sub-agent or workflow
// still "running" because the app was closed or crashed mid-turn) in
// sessionID and its descendant sessions. It is fire-and-forget: the
// coordinator's writes flow back through the same pubsub events a
// live run would emit, so the chat updates on its own once each
// write lands, matching how a live cancellation is reflected today.
func (m *UI) reconcileStuckSession(sessionID string) tea.Cmd {
	return func() tea.Msg {
		if _, err := m.com.Workspace.AgentReconcileStuckSession(context.Background(), sessionID); err != nil {
			slog.Debug("Failed to reconcile stuck sub-agent/workflow tool calls", "session_id", sessionID, "error", err)
		}
		return nil
	}
}

// syncTmuxSession updates tmux pane user options with the current
// session's ID and title, when tmux integration is enabled and Crush is
// running inside tmux. This lets external tooling (e.g. a
// tmux-resurrect restore hook) relaunch Crush attached to the same
// session after a tmux server restart. The call is fire-and-forget:
// errors are logged at debug and never surfaced to the user.
func (m *UI) syncTmuxSession() tea.Cmd {
	if m.session == nil {
		return nil
	}
	cfg := m.com.Config()
	if cfg == nil || cfg.Options == nil || !cfg.Options.TmuxIntegration {
		return nil
	}
	if !tmux.Available() {
		return nil
	}
	sessionID, title := m.session.ID, m.session.Title
	cwd := m.com.Workspace.WorkingDir()
	return func() tea.Msg {
		ctx := context.Background()
		if err := tmux.SetSessionID(ctx, sessionID); err != nil {
			slog.Debug("Failed to set tmux session id", "error", err)
		}
		if err := tmux.SetSessionTitle(ctx, title); err != nil {
			slog.Debug("Failed to set tmux session title", "error", err)
		}
		if err := tmux.RecordSession(ctx, cwd, sessionID, title); err != nil {
			slog.Debug("Failed to record tmux session mapping", "error", err)
		}
		return nil
	}
}

func (m *UI) loadSessionFiles(sessionID string) ([]SessionFile, error) {
	files, err := m.com.Workspace.ListSessionHistory(context.Background(), sessionID)
	if err != nil {
		return nil, err
	}

	filesByPath := make(map[string][]history.File)
	for _, f := range files {
		filesByPath[f.Path] = append(filesByPath[f.Path], f)
	}
	sessionFiles := make([]SessionFile, 0, len(filesByPath))
	for _, versions := range filesByPath {
		if len(versions) == 0 {
			continue
		}

		first := versions[0]
		last := versions[0]
		for _, v := range versions {
			if v.Version < first.Version {
				first = v
			}
			if v.Version > last.Version {
				last = v
			}
		}

		_, additions, deletions := diff.GenerateDiff(first.Content, last.Content, first.Path)

		sessionFiles = append(sessionFiles, SessionFile{
			FirstVersion:  first,
			LatestVersion: last,
			Additions:     additions,
			Deletions:     deletions,
		})
	}

	slices.SortFunc(sessionFiles, func(a, b SessionFile) int {
		if a.LatestVersion.UpdatedAt > b.LatestVersion.UpdatedAt {
			return -1
		}
		if a.LatestVersion.UpdatedAt < b.LatestVersion.UpdatedAt {
			return 1
		}
		return 0
	})
	return sessionFiles, nil
}

// handleFileEvent processes file change events and updates the session file
// list with new or updated file information.
func (m *UI) handleFileEvent(file history.File) tea.Cmd {
	if m.session == nil || file.SessionID != m.session.ID {
		return nil
	}

	return func() tea.Msg {
		sessionFiles, err := m.loadSessionFiles(m.session.ID)
		// could not load session files
		if err != nil {
			return util.NewErrorMsg(err)
		}

		return sessionFilesUpdatesMsg{
			sessionFiles: sessionFiles,
		}
	}
}

// filesInfo renders the modified files section for the sidebar, showing files
// with their addition/deletion counts.
func (m *UI) filesInfo(cwd string, width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Files.SectionTitle.Render("Modified Files")
	if isSection {
		title = common.Section(t, "Modified Files", width)
	}
	list := t.Files.EmptyMessage.Render("None")
	var filesWithChanges []SessionFile
	for _, f := range m.sessionFiles {
		if f.Additions == 0 && f.Deletions == 0 {
			continue
		}
		filesWithChanges = append(filesWithChanges, f)
	}
	if len(filesWithChanges) > 0 {
		list = fileList(t, cwd, filesWithChanges, width, maxItems)
	}

	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

// fileList renders a list of files with their diff statistics, truncating to
// maxItems and showing a "...and N more" message if needed.
func fileList(t *styles.Styles, cwd string, filesWithChanges []SessionFile, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}
	var renderedFiles []string
	filesShown := 0

	for _, f := range filesWithChanges {
		// Skip files with no changes
		if filesShown >= maxItems {
			break
		}

		// Build stats string with colors
		var statusParts []string
		if f.Additions > 0 {
			statusParts = append(statusParts, t.Files.Additions.Render(fmt.Sprintf("+%d", f.Additions)))
		}
		if f.Deletions > 0 {
			statusParts = append(statusParts, t.Files.Deletions.Render(fmt.Sprintf("-%d", f.Deletions)))
		}
		extraContent := strings.Join(statusParts, " ")

		// Format file path
		filePath := f.FirstVersion.Path
		if rel, err := filepath.Rel(cwd, filePath); err == nil {
			filePath = rel
		}
		filePath = fsext.DirTrim(filePath, 2)
		suffix := ""
		if extraContent != "" {
			suffix = " " + extraContent
		}
		maxPathWidth := max(width-lipgloss.Width(suffix), 0)
		filePath = ansi.Truncate(filePath, maxPathWidth, "…")

		line := t.Files.Path.Render(filePath)
		if extraContent != "" {
			line = fmt.Sprintf("%s %s", line, extraContent)
		}

		renderedFiles = append(renderedFiles, line)
		filesShown++
	}

	if len(filesWithChanges) > maxItems {
		remaining := len(filesWithChanges) - maxItems
		renderedFiles = append(renderedFiles, t.Files.TruncationHint.Render(fmt.Sprintf("…and %d more", remaining)))
	}

	return lipgloss.JoinVertical(lipgloss.Left, renderedFiles...)
}

// startLSPs starts LSP servers for the given file paths.
func (m *UI) startLSPs(paths []string) tea.Cmd {
	if len(paths) == 0 {
		return nil
	}

	return func() tea.Msg {
		ctx := context.Background()
		for _, path := range paths {
			m.com.Workspace.LSPStart(ctx, path)
		}
		return nil
	}
}
