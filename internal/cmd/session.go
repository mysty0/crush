package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/charmtone"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

var sessionCmd = &cobra.Command{
	Use:     "session",
	Aliases: []string{"sessions", "s"},
	Short:   "Manage sessions",
	Long:    "Manage Crush sessions. Agents can use --json for machine-readable output.",
}

var (
	sessionListJSON   bool
	sessionShowJSON   bool
	sessionLastJSON   bool
	sessionDeleteJSON bool
	sessionRenameJSON bool
)

var sessionListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all sessions",
	Long:    "List all sessions. Use --json for machine-readable output.",
	RunE:    runSessionList,
}

var sessionShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show session details",
	Long:  "Show session details. Use --json for machine-readable output. ID can be a UUID, full hash, or hash prefix.",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionShow,
}

var sessionLastCmd = &cobra.Command{
	Use:   "last",
	Short: "Show most recent session",
	Long:  "Show the last updated session. Use --json for machine-readable output.",
	RunE:  runSessionLast,
}

var sessionDeleteCmd = &cobra.Command{
	Use:     "delete <id>",
	Aliases: []string{"rm"},
	Short:   "Delete a session",
	Long:    "Delete a session by ID. Use --json for machine-readable output. ID can be a UUID, full hash, or hash prefix.",
	Args:    cobra.ExactArgs(1),
	RunE:    runSessionDelete,
}

var sessionRenameCmd = &cobra.Command{
	Use:   "rename <id> <title>",
	Short: "Rename a session",
	Long:  "Rename a session by ID. Use --json for machine-readable output. ID can be a UUID, full hash, or hash prefix.",
	Args:  cobra.MinimumNArgs(2),
	RunE:  runSessionRename,
}

func init() {
	sessionListCmd.Flags().BoolVar(&sessionListJSON, "json", false, "output in JSON format")
	sessionShowCmd.Flags().BoolVar(&sessionShowJSON, "json", false, "output in JSON format")
	sessionLastCmd.Flags().BoolVar(&sessionLastJSON, "json", false, "output in JSON format")
	sessionDeleteCmd.Flags().BoolVar(&sessionDeleteJSON, "json", false, "output in JSON format")
	sessionRenameCmd.Flags().BoolVar(&sessionRenameJSON, "json", false, "output in JSON format")
	sessionCmd.AddCommand(sessionListCmd)
	sessionCmd.AddCommand(sessionShowCmd)
	sessionCmd.AddCommand(sessionLastCmd)
	sessionCmd.AddCommand(sessionDeleteCmd)
	sessionCmd.AddCommand(sessionRenameCmd)
}

type sessionServices struct {
	sessions session.Service
	messages message.Service
	cfg      *config.ConfigStore
}

func sessionSetup(cmd *cobra.Command) (context.Context, *sessionServices, func(), error) {
	dataDir, _ := cmd.Flags().GetString("data-dir")
	ctx := cmd.Context()

	cfg, err := config.Init("", dataDir, false)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to initialize config: %w", err)
	}
	if dataDir == "" {
		dataDir = cfg.Config().Options.DataDirectory
	}
	if shouldEnableMetrics(cfg.Config()) {
		event.Init()
	}

	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	queries := db.New(conn)
	svc := &sessionServices{
		sessions: session.NewService(queries, conn),
		messages: message.NewService(queries, conn),
		cfg:      cfg,
	}
	return ctx, svc, func() { conn.Close() }, nil
}

func runSessionList(cmd *cobra.Command, _ []string) error {
	event.SetNonInteractive(true)

	ctx, svc, cleanup, err := sessionSetup(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	event.SessionListed(sessionListJSON)

	list, err := svc.sessions.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if sessionListJSON {
		out := cmd.OutOrStdout()
		output := make([]sessionJSON, len(list))
		for i, s := range list {
			output[i] = sessionJSON{
				ID:       session.HashID(s.ID),
				UUID:     s.ID,
				Title:    s.Title,
				Created:  time.Unix(s.CreatedAt, 0).Format(time.RFC3339),
				Modified: time.Unix(s.UpdatedAt, 0).Format(time.RFC3339),
			}
		}
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		return enc.Encode(output)
	}

	w, cleanup, usingPager := sessionWriter(ctx, len(list))
	defer cleanup()

	hashStyle := lipgloss.NewStyle().Foreground(charmtone.Malibu)
	dateStyle := lipgloss.NewStyle().Foreground(charmtone.Damson)

	width := sessionOutputWidth
	if tw, _, err := term.GetSize(os.Stdout.Fd()); err == nil && tw > 0 {
		width = tw
	}
	// 7 (hash) + 1 (space) + 25 (RFC3339 date) + 1 (space) = 34 chars prefix.
	titleWidth := max(width-34, 10)

	var writeErr error
	for _, s := range list {
		hash := session.HashID(s.ID)[:7]
		date := time.Unix(s.CreatedAt, 0).Format(time.RFC3339)
		title := strings.ReplaceAll(s.Title, "\n", " ")
		title = ansi.Truncate(title, titleWidth, "…")
		_, writeErr = fmt.Fprintln(w, hashStyle.Render(hash), dateStyle.Render(date), title)
		if writeErr != nil {
			break
		}
	}
	if writeErr != nil && usingPager && isBrokenPipe(writeErr) {
		return nil
	}
	return writeErr
}

type sessionJSON struct {
	ID       string `json:"id"`
	UUID     string `json:"uuid"`
	Title    string `json:"title"`
	Created  string `json:"created"`
	Modified string `json:"modified"`
}

type sessionMutationResult struct {
	ID      string `json:"id"`
	UUID    string `json:"uuid"`
	Title   string `json:"title"`
	Deleted bool   `json:"deleted,omitempty"`
	Renamed bool   `json:"renamed,omitempty"`
}

// resolveSessionID resolves a session ID that can be a UUID, full hash, or hash prefix.
// Returns an error if the prefix is ambiguous (matches multiple sessions).
func resolveSessionID(ctx context.Context, svc session.Service, id string) (session.Session, error) {
	// Try direct UUID lookup first
	if s, err := svc.Get(ctx, id); err == nil {
		return s, nil
	}

	// List all sessions and check for hash matches
	sessions, err := svc.List(ctx)
	if err != nil {
		return session.Session{}, err
	}

	var matches []session.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	if len(matches) == 0 {
		return session.Session{}, fmt.Errorf("session not found: %s", id)
	}

	if len(matches) == 1 {
		return matches[0], nil
	}

	// Ambiguous - show matches like Git does
	var sb strings.Builder
	fmt.Fprintf(&sb, "session ID '%s' is ambiguous. Matches:\n\n", id)
	for _, m := range matches {
		hash := session.HashID(m.ID)
		created := time.Unix(m.CreatedAt, 0).Format("2006-01-02")
		// Keep title on one line by replacing newlines with spaces, and truncate.
		title := strings.ReplaceAll(m.Title, "\n", " ")
		title = ansi.Truncate(title, 50, "…")
		fmt.Fprintf(&sb, "  %s... %q (created %s)\n", hash[:12], title, created)
	}
	sb.WriteString("\nUse more characters or the full hash")
	return session.Session{}, errors.New(sb.String())
}

func runSessionShow(cmd *cobra.Command, args []string) error {
	event.SetNonInteractive(true)

	ctx, svc, cleanup, err := sessionSetup(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	event.SessionShown(sessionShowJSON)

	sess, err := resolveSessionID(ctx, svc.sessions, args[0])
	if err != nil {
		return err
	}

	msgs, err := svc.messages.List(ctx, sess.ID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	msgPtrs := messagePtrs(msgs)
	if sessionShowJSON {
		return outputSessionJSON(ctx, svc, cmd.OutOrStdout(), sess, msgPtrs)
	}
	return outputSessionHuman(ctx, svc, sess, msgPtrs)
}

func runSessionDelete(cmd *cobra.Command, args []string) error {
	event.SetNonInteractive(true)

	ctx, svc, cleanup, err := sessionSetup(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	event.SessionDeletedCommand(sessionDeleteJSON)

	sess, err := resolveSessionID(ctx, svc.sessions, args[0])
	if err != nil {
		return err
	}

	if err := svc.sessions.Delete(ctx, sess.ID); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	out := cmd.OutOrStdout()
	if sessionDeleteJSON {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		return enc.Encode(sessionMutationResult{
			ID:      session.HashID(sess.ID),
			UUID:    sess.ID,
			Title:   sess.Title,
			Deleted: true,
		})
	}

	fmt.Fprintf(out, "Deleted session %s\n", session.HashID(sess.ID)[:12])
	return nil
}

func runSessionRename(cmd *cobra.Command, args []string) error {
	event.SetNonInteractive(true)

	ctx, svc, cleanup, err := sessionSetup(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	event.SessionRenamed(sessionRenameJSON)

	sess, err := resolveSessionID(ctx, svc.sessions, args[0])
	if err != nil {
		return err
	}

	newTitle := strings.Join(args[1:], " ")
	if err := svc.sessions.Rename(ctx, sess.ID, newTitle); err != nil {
		return fmt.Errorf("failed to rename session: %w", err)
	}

	out := cmd.OutOrStdout()
	if sessionRenameJSON {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		return enc.Encode(sessionMutationResult{
			ID:      session.HashID(sess.ID),
			UUID:    sess.ID,
			Title:   newTitle,
			Renamed: true,
		})
	}

	fmt.Fprintf(out, "Renamed session %s to %q\n", session.HashID(sess.ID)[:12], newTitle)
	return nil
}

func runSessionLast(cmd *cobra.Command, _ []string) error {
	event.SetNonInteractive(true)

	ctx, svc, cleanup, err := sessionSetup(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	event.SessionLastShown(sessionLastJSON)

	list, err := svc.sessions.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(list) == 0 {
		return fmt.Errorf("no sessions found")
	}

	sess := list[0]

	msgs, err := svc.messages.List(ctx, sess.ID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	msgPtrs := messagePtrs(msgs)
	if sessionLastJSON {
		return outputSessionJSON(ctx, svc, cmd.OutOrStdout(), sess, msgPtrs)
	}
	return outputSessionHuman(ctx, svc, sess, msgPtrs)
}

const (
	sessionOutputWidth     = 80
	sessionMaxContentWidth = 120
)

func messagePtrs(msgs []message.Message) []*message.Message {
	ptrs := make([]*message.Message, len(msgs))
	for i := range msgs {
		ptrs[i] = &msgs[i]
	}
	return ptrs
}

// maxSubAgentDepth bounds how many levels of nested sub-agent
// conversations (e.g. an "agent" tool call whose transcript itself
// contains another "agent" tool call) are followed and embedded. This
// only guards against pathological/circular data; real conversations
// rarely nest more than one or two levels deep.
const maxSubAgentDepth = 8

// isSubAgentToolName reports whether name is a tool that dispatches a
// sub-agent turn with its own session (see agent.runSubAgent), whose
// transcript should be discoverable from the parent session.
func isSubAgentToolName(name string) bool {
	return name == agent.AgentToolName || name == tools.AgenticFetchToolName
}

// isWorkflowToolName reports whether name is the Workflow tool, whose
// dedicated session parents a tree of per-phase sub-agent sessions that
// must be discovered by parent rather than by tool_call part.
func isWorkflowToolName(name string) bool {
	return name == agent.WorkflowToolName
}

func outputSessionJSON(ctx context.Context, svc *sessionServices, w io.Writer, sess session.Session, msgs []*message.Message) error {
	skills := extractSkillsFromMessages(msgs)
	output := sessionShowOutput{
		Meta: sessionShowMeta{
			ID:                  session.HashID(sess.ID),
			UUID:                sess.ID,
			Title:               sess.Title,
			Created:             time.Unix(sess.CreatedAt, 0).Format(time.RFC3339),
			Modified:            time.Unix(sess.UpdatedAt, 0).Format(time.RFC3339),
			Cost:                sess.Cost,
			PromptTokens:        sess.PromptTokens,
			CompletionTokens:    sess.CompletionTokens,
			CacheCreationTokens: sess.CacheCreationTokens,
			CacheReadTokens:     sess.CacheReadTokens,
			TotalTokens:         sess.PromptTokens + sess.CompletionTokens,
			Skills:              skills,
		},
		Messages: buildSessionShowMessages(ctx, svc, msgs, maxSubAgentDepth),
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(output)
}

// buildSessionShowMessages converts msgs into their JSON representation,
// recursively embedding any sub-agent tool call's own conversation
// (up to depth levels) inline via sessionShowPart.SubAgent. Without
// this, a "session show" of a session that spawned a sub-agent (e.g.
// via the "agent" tool) would not surface what that sub-agent
// actually did: its transcript lives in a separate child session that
// isn't listed by "session list" and has no other discovery path.
func buildSessionShowMessages(ctx context.Context, svc *sessionServices, msgs []*message.Message, depth int) []sessionShowMessage {
	out := make([]sessionShowMessage, len(msgs))
	for i, msg := range msgs {
		out[i] = sessionShowMessage{
			ID:       msg.ID,
			Role:     string(msg.Role),
			Created:  time.Unix(msg.CreatedAt, 0).Format(time.RFC3339),
			Model:    msg.Model,
			Provider: msg.Provider,
			Parts:    convertParts(msg.Parts),
		}
		if depth <= 0 {
			continue
		}
		for pi, part := range out[i].Parts {
			if part.Type != "tool_call" {
				continue
			}
			switch {
			case isSubAgentToolName(part.Name):
				subSessionID := svc.sessions.CreateAgentToolSessionID(msg.ID, part.ToolCallID)
				subMsgs, err := svc.messages.List(ctx, subSessionID)
				if err != nil || len(subMsgs) == 0 {
					continue
				}
				out[i].Parts[pi].SubAgent = &sessionShowSubAgent{
					SessionID: subSessionID,
					Messages:  buildSessionShowMessages(ctx, svc, messagePtrs(subMsgs), depth-1),
				}
			case isWorkflowToolName(part.Name):
				// A workflow's own dedicated session has no LLM
				// messages of its own; its work lives in a tree of
				// per-phase sub-agent sessions parented under it. Embed
				// each child sub-agent's transcript so "session show"
				// surfaces everything the workflow did.
				workflowSessionID := svc.sessions.CreateAgentToolSessionID(msg.ID, part.ToolCallID)
				children, err := svc.sessions.ListChildren(ctx, workflowSessionID)
				if err != nil || len(children) == 0 {
					continue
				}
				out[i].Parts[pi].WorkflowAgents = buildWorkflowAgentShows(ctx, svc, children, depth-1)
			}
		}
	}
	return out
}

// loadNestedToolItems populates the nested transcript on any sub-agent
// tool call items (e.g. "agent"/"agentic_fetch") found in items, up to
// depth levels, so "session show" renders a sub-agent's activity
// inline the same way the TUI does (see UI.loadNestedToolCalls),
// rather than showing just a bare, collapsed tool call.
func loadNestedToolItems(ctx context.Context, svc *sessionServices, sty *styles.Styles, items []chat.MessageItem, depth int) {
	if depth <= 0 {
		return
	}
	for _, item := range items {
		nestedContainer, ok := item.(chat.NestedToolContainer)
		if !ok {
			continue
		}
		toolItem, ok := item.(chat.ToolMessageItem)
		if !ok {
			continue
		}

		tc := toolItem.ToolCall()
		subSessionID := svc.sessions.CreateAgentToolSessionID(toolItem.MessageID(), tc.ID)

		subMsgs, err := svc.messages.List(ctx, subSessionID)
		if err != nil || len(subMsgs) == 0 {
			continue
		}

		subMsgPtrs := messagePtrs(subMsgs)
		subToolResults := chat.BuildToolResultMap(subMsgPtrs)

		var nestedTools []chat.ToolMessageItem
		for _, subMsg := range subMsgPtrs {
			subItems := chat.ExtractMessageItems(sty, subMsg, subToolResults)
			for _, subItem := range subItems {
				if nestedToolItem, ok := subItem.(chat.ToolMessageItem); ok {
					if compactable, ok := nestedToolItem.(chat.Compactable); ok {
						compactable.SetCompact(true)
					}
					nestedTools = append(nestedTools, nestedToolItem)
				}
			}
		}

		nestedMessageItems := make([]chat.MessageItem, len(nestedTools))
		for i, nt := range nestedTools {
			nestedMessageItems[i] = nt
		}
		loadNestedToolItems(ctx, svc, sty, nestedMessageItems, depth-1)

		nestedContainer.SetNestedTools(nestedTools)
	}
}

func outputSessionHuman(ctx context.Context, svc *sessionServices, sess session.Session, msgs []*message.Message) error {
	var providerID string
	if svc.cfg != nil {
		providerID = svc.cfg.Config().Models[config.SelectedModelTypeLarge].Provider
	}
	styles := styles.ThemeForProvider(providerID)
	toolResults := chat.BuildToolResultMap(msgs)

	width := sessionOutputWidth
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		width = w
	}
	contentWidth := min(width, sessionMaxContentWidth)

	keyStyle := lipgloss.NewStyle().Foreground(charmtone.Damson)
	valStyle := lipgloss.NewStyle().Foreground(charmtone.Malibu)

	hash := session.HashID(sess.ID)[:12]
	created := time.Unix(sess.CreatedAt, 0).Format("Mon Jan 2 15:04:05 2006 -0700")

	skills := extractSkillsFromMessages(msgs)

	// Render to buffer to determine actual height
	var buf strings.Builder

	fmt.Fprintln(&buf, keyStyle.Render("ID:    ")+valStyle.Render(hash))
	fmt.Fprintln(&buf, keyStyle.Render("UUID:  ")+valStyle.Render(sess.ID))
	fmt.Fprintln(&buf, keyStyle.Render("Title: ")+valStyle.Render(sess.Title))
	fmt.Fprintln(&buf, keyStyle.Render("Date:  ")+valStyle.Render(created))
	if len(skills) > 0 {
		skillNames := make([]string, len(skills))
		for i, s := range skills {
			timestamp := time.Unix(sess.CreatedAt, 0).Format("15:04:05 -0700")
			if s.LoadedAt != "" {
				if t, err := time.Parse(time.RFC3339, s.LoadedAt); err == nil {
					timestamp = t.Format("15:04:05 -0700")
				}
			}
			skillNames[i] = fmt.Sprintf("%s (%s)", s.Name, timestamp)
		}
		fmt.Fprintln(&buf, keyStyle.Render("Skills: ")+valStyle.Render(strings.Join(skillNames, ", ")))
	}
	fmt.Fprintln(&buf)

	first := true
	for _, msg := range msgs {
		items := chat.ExtractMessageItems(&styles, msg, toolResults)
		loadNestedToolItems(ctx, svc, &styles, items, maxSubAgentDepth)
		for _, item := range items {
			if !first {
				fmt.Fprintln(&buf)
			}
			first = false
			fmt.Fprintln(&buf, item.Render(contentWidth))
		}
	}
	fmt.Fprintln(&buf)

	contentHeight := strings.Count(buf.String(), "\n")
	w, cleanup, usingPager := sessionWriter(ctx, contentHeight)
	defer cleanup()

	_, err := io.WriteString(w, buf.String())
	// Ignore broken pipe errors when using a pager. This happens when the user
	// exits the pager early (e.g., pressing 'q' in less), which closes the pipe
	// and causes subsequent writes to fail. These errors are expected user behavior.
	if err != nil && usingPager && isBrokenPipe(err) {
		return nil
	}
	return err
}

func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	// Check for syscall.EPIPE (broken pipe)
	if errors.Is(err, syscall.EPIPE) {
		return true
	}
	// Also check for "broken pipe" in the error message
	return strings.Contains(err.Error(), "broken pipe")
}

// sessionWriter returns a writer, cleanup function, and a bool indicating if a pager is used.
// When the content fits within the terminal (or stdout is not a TTY), it returns
// a colorprofile.Writer wrapping stdout. When content exceeds terminal height,
// it starts a pager process (respecting $PAGER, defaulting to "less -R").
func sessionWriter(ctx context.Context, contentHeight int) (io.Writer, func(), bool) {
	// Use NewWriter which automatically detects TTY and strips ANSI when redirected
	if runtime.GOOS == "windows" || !term.IsTerminal(os.Stdout.Fd()) {
		return colorprofile.NewWriter(os.Stdout, os.Environ()), func() {}, false
	}

	_, termHeight, err := term.GetSize(os.Stdout.Fd())
	if err != nil || contentHeight <= termHeight {
		return colorprofile.NewWriter(os.Stdout, os.Environ()), func() {}, false
	}

	// Detect color profile from stderr since stdout is piped to the pager.
	profile := colorprofile.Detect(os.Stderr, os.Environ())

	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less -R"
	}

	parts := strings.Fields(pager)
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...) //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	pipe, err := cmd.StdinPipe()
	if err != nil {
		return colorprofile.NewWriter(os.Stdout, os.Environ()), func() {}, false
	}

	if err := cmd.Start(); err != nil {
		return colorprofile.NewWriter(os.Stdout, os.Environ()), func() {}, false
	}

	return &colorprofile.Writer{
			Forward: pipe,
			Profile: profile,
		}, func() {
			pipe.Close()
			_ = cmd.Wait()
		}, true
}

type sessionShowMeta struct {
	ID                  string             `json:"id"`
	UUID                string             `json:"uuid"`
	Title               string             `json:"title"`
	Created             string             `json:"created"`
	Modified            string             `json:"modified"`
	Cost                float64            `json:"cost"`
	PromptTokens        int64              `json:"prompt_tokens"`
	CompletionTokens    int64              `json:"completion_tokens"`
	CacheCreationTokens int64              `json:"cache_creation_tokens"`
	CacheReadTokens     int64              `json:"cache_read_tokens"`
	TotalTokens         int64              `json:"total_tokens"`
	Skills              []sessionShowSkill `json:"skills,omitempty"`
}

type sessionShowSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	LoadedAt    string `json:"loaded_at"`
}

type sessionShowMessage struct {
	ID       string            `json:"id"`
	Role     string            `json:"role"`
	Created  string            `json:"created"`
	Model    string            `json:"model,omitempty"`
	Provider string            `json:"provider,omitempty"`
	Parts    []sessionShowPart `json:"parts"`
}

type sessionShowPart struct {
	Type string `json:"type"`

	// Text content
	Text string `json:"text,omitempty"`

	// Reasoning
	Thinking   string `json:"thinking,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`

	// Tool call
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Input      string `json:"input,omitempty"`

	// SubAgent is populated on "agent"/"agentic_fetch" tool_call parts
	// with the sub-agent's own conversation, embedded inline so it's
	// discoverable without knowing its (otherwise-hidden) child session
	// ID. See buildSessionShowMessages.
	SubAgent *sessionShowSubAgent `json:"sub_agent,omitempty"`

	// WorkflowAgents is populated on "Workflow" tool_call parts with
	// every per-phase sub-agent the workflow spawned, embedded inline
	// so a workflow's full activity is discoverable from the parent
	// session. See buildSessionShowMessages.
	WorkflowAgents []sessionShowWorkflowAgent `json:"workflow_agents,omitempty"`

	// Tool result
	Content  string `json:"content,omitempty"`
	IsError  bool   `json:"is_error,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`

	// Binary
	Size int64 `json:"size,omitempty"`

	// Image URL
	URL    string `json:"url,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Finish
	Reason string `json:"reason,omitempty"`
	Time   int64  `json:"time,omitempty"`
}

// sessionShowSubAgent holds a sub-agent's own conversation, embedded
// inline on the tool_call part that spawned it.
type sessionShowSubAgent struct {
	SessionID string               `json:"session_id"`
	Messages  []sessionShowMessage `json:"messages"`
}

// sessionShowWorkflowAgent holds one workflow-dispatched sub-agent's
// stats and conversation, embedded inline on the Workflow tool_call
// part. The phase is derived from the sub-session title.
type sessionShowWorkflowAgent struct {
	SessionID           string               `json:"session_id"`
	Title               string               `json:"title"`
	PromptTokens        int64                `json:"prompt_tokens"`
	CompletionTokens    int64                `json:"completion_tokens"`
	CacheCreationTokens int64                `json:"cache_creation_tokens"`
	CacheReadTokens     int64                `json:"cache_read_tokens"`
	Cost                float64              `json:"cost"`
	Messages            []sessionShowMessage `json:"messages"`
}

// buildWorkflowAgentShows converts a workflow's child sessions into
// their JSON representation with per-agent stats and transcripts.
func buildWorkflowAgentShows(ctx context.Context, svc *sessionServices, children []session.Session, depth int) []sessionShowWorkflowAgent {
	out := make([]sessionShowWorkflowAgent, 0, len(children))
	for _, child := range children {
		msgs, err := svc.messages.List(ctx, child.ID)
		if err != nil {
			continue
		}
		var childMessages []sessionShowMessage
		if depth > 0 {
			childMessages = buildSessionShowMessages(ctx, svc, messagePtrs(msgs), depth)
		}
		out = append(out, sessionShowWorkflowAgent{
			SessionID:           child.ID,
			Title:               child.Title,
			PromptTokens:        child.PromptTokens,
			CompletionTokens:    child.CompletionTokens,
			CacheCreationTokens: child.CacheCreationTokens,
			CacheReadTokens:     child.CacheReadTokens,
			Cost:                child.Cost,
			Messages:            childMessages,
		})
	}
	return out
}

func extractSkillsFromMessages(msgs []*message.Message) []sessionShowSkill {
	var skills []sessionShowSkill
	seen := make(map[string]bool)

	for _, msg := range msgs {
		for _, part := range msg.Parts {
			if tr, ok := part.(message.ToolResult); ok && tr.Metadata != "" {
				var meta tools.ViewResponseMetadata
				if err := json.Unmarshal([]byte(tr.Metadata), &meta); err == nil {
					if meta.ResourceType == tools.ViewResourceSkill && meta.ResourceName != "" {
						if !seen[meta.ResourceName] {
							seen[meta.ResourceName] = true
							skills = append(skills, sessionShowSkill{
								Name:        meta.ResourceName,
								Description: meta.ResourceDescription,
								LoadedAt:    time.Unix(msg.CreatedAt, 0).Format(time.RFC3339),
							})
						}
					}
				}
			}
		}
	}

	sort.Slice(skills, func(i, j int) bool {
		if skills[i].LoadedAt == skills[j].LoadedAt {
			return skills[i].Name < skills[j].Name
		}
		return skills[i].LoadedAt < skills[j].LoadedAt
	})

	return skills
}

func convertParts(parts []message.ContentPart) []sessionShowPart {
	result := make([]sessionShowPart, 0, len(parts))
	for _, part := range parts {
		switch p := part.(type) {
		case message.TextContent:
			result = append(result, sessionShowPart{
				Type: "text",
				Text: p.Text,
			})
		case message.ReasoningContent:
			result = append(result, sessionShowPart{
				Type:       "reasoning",
				Thinking:   p.Thinking,
				StartedAt:  p.StartedAt,
				FinishedAt: p.FinishedAt,
			})
		case message.ToolCall:
			result = append(result, sessionShowPart{
				Type:       "tool_call",
				ToolCallID: p.ID,
				Name:       p.Name,
				Input:      p.Input,
			})
		case message.ToolResult:
			result = append(result, sessionShowPart{
				Type:       "tool_result",
				ToolCallID: p.ToolCallID,
				Name:       p.Name,
				Content:    p.Content,
				IsError:    p.IsError,
				MIMEType:   p.MIMEType,
			})
		case message.BinaryContent:
			result = append(result, sessionShowPart{
				Type:     "binary",
				MIMEType: p.MIMEType,
				Size:     int64(len(p.Data)),
			})
		case message.ImageURLContent:
			result = append(result, sessionShowPart{
				Type:   "image_url",
				URL:    p.URL,
				Detail: p.Detail,
			})
		case message.Finish:
			result = append(result, sessionShowPart{
				Type:   "finish",
				Reason: string(p.Reason),
				Time:   p.Time,
			})
		default:
			result = append(result, sessionShowPart{
				Type: "unknown",
			})
		}
	}
	return result
}

type sessionShowOutput struct {
	Meta     sessionShowMeta      `json:"meta"`
	Messages []sessionShowMessage `json:"messages"`
}
