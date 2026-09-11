package model

import (
	"testing"

	"charm.land/bubbles/v2/textarea"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/workspace"
)

// taskListWorkspace is a workspace stub whose only job is to serve a
// fixed background-task list.
type taskListWorkspace struct {
	workspace.Workspace

	tasks []agent.TaskStatus
}

func (w *taskListWorkspace) AgentTasks(string) []agent.TaskStatus { return w.tasks }

func (w *taskListWorkspace) WorkingDir() string { return "" }

func (w *taskListWorkspace) Config() *config.Config { return nil }

// newSubAgentViewUI builds a UI parked in the fullscreen sub-agent view
// for sessionID, with the given background tasks visible.
func newSubAgentViewUI(sessionID string, tasks []agent.TaskStatus) *UI {
	ws := &taskListWorkspace{tasks: tasks}
	com := common.DefaultCommon(ws)
	m := &UI{
		com:               com,
		status:            NewStatus(com, nil),
		chat:              NewChat(com, config.ScrollbarDefault),
		textarea:          textarea.New(),
		state:             uiChat,
		focus:             uiFocusEditor,
		width:             140,
		height:            45,
		session:           &session.Session{ID: "s1"},
		keyMap:            DefaultKeyMap(),
		dialog:            dialog.NewOverlay(),
		attachments:       attachments.New(nil, attachments.Keymap{}),
		subAgentSessionID: sessionID,
	}
	return m
}

// TestSubAgentViewExitsWhenTaskFinishes pins the escape hatch out of the
// fullscreen sub-agent view: a backgrounded sub-agent's tool call has
// already returned, so nothing else closes the view when it finishes and
// the user would be stranded in a dead view.
func TestSubAgentViewExitsWhenTaskFinishes(t *testing.T) {
	t.Parallel()

	ref := agent.TaskRef{Kind: agent.TaskKindSubAgent, ID: "sub-1"}

	t.Run("running sub-agent keeps the view", func(t *testing.T) {
		t.Parallel()
		m := newSubAgentViewUI("sub-1", []agent.TaskStatus{{Ref: ref, State: agent.TaskRunning}})
		require.Nil(t, m.maybeExitFinishedSubAgentView(ref))
		require.Equal(t, "sub-1", m.subAgentSessionID)
	})

	t.Run("finished sub-agent snaps back to main", func(t *testing.T) {
		t.Parallel()
		m := newSubAgentViewUI("sub-1", []agent.TaskStatus{{Ref: ref, State: agent.TaskDone}})
		require.NotNil(t, m.maybeExitFinishedSubAgentView(ref))
		require.Empty(t, m.subAgentSessionID)
	})

	t.Run("unknown task snaps back to main", func(t *testing.T) {
		t.Parallel()
		// A workflow's agent is owned by the workflow session, so it
		// never shows in this session's task list; its finish event is
		// still the signal to leave the view.
		m := newSubAgentViewUI("sub-1", nil)
		require.NotNil(t, m.maybeExitFinishedSubAgentView(ref))
		require.Empty(t, m.subAgentSessionID)
	})

	t.Run("another task finishing is ignored", func(t *testing.T) {
		t.Parallel()
		m := newSubAgentViewUI("sub-1", nil)
		other := agent.TaskRef{Kind: agent.TaskKindSubAgent, ID: "sub-2"}
		require.Nil(t, m.maybeExitFinishedSubAgentView(other))
		require.Equal(t, "sub-1", m.subAgentSessionID)
	})

	t.Run("non sub-agent kinds are ignored", func(t *testing.T) {
		t.Parallel()
		m := newSubAgentViewUI("sub-1", nil)
		bash := agent.TaskRef{Kind: agent.TaskKindBash, ID: "sub-1"}
		require.Nil(t, m.maybeExitFinishedSubAgentView(bash))
		require.Equal(t, "sub-1", m.subAgentSessionID)
	})
}
