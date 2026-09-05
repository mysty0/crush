package dialog

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/workspace"
)

// reloadWorkspaceStub is a workspace.Workspace stub exposing only what the
// Models dialog's background reload touches. The embedded interface is
// nil and panics on anything else, so a test that reaches further than
// Config/ReloadConfigIfStale fails loudly instead of silently misusing
// unconfigured behavior.
type reloadWorkspaceStub struct {
	workspace.Workspace

	cfg          *config.Config
	reloadCalls  int
	reloadResult bool
	reloadErr    error
}

func (w *reloadWorkspaceStub) Config() *config.Config { return w.cfg }

func (w *reloadWorkspaceStub) ReloadConfigIfStale(context.Context) (bool, error) {
	w.reloadCalls++
	return w.reloadResult, w.reloadErr
}

// newReloadTestConfig builds a minimal config with default provider
// discovery disabled, so constructing a Models dialog never reaches out
// to the network for a provider catalog.
func newReloadTestConfig() *config.Config {
	return &config.Config{
		Options:   &config.Options{DisableDefaultProviders: true},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}
}

func newReloadTestModels(t *testing.T, ws *reloadWorkspaceStub) *Models {
	t.Helper()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s, Workspace: ws}
	m, err := NewModels(com, false)
	require.NoError(t, err)
	return m
}

// TestOpenModelsDialogDoesNotBlockOnReload pins the fix for a real report:
// opening the model switcher used to call ReloadConfigIfStale synchronously
// before building the dialog, so a stale config triggering a full reload
// (which re-queries provider model lists over the network) made the
// dialog take seconds to appear. The dialog itself must not reload
// eagerly at construction time -- StartLoading is what kicks off the
// background check.
func TestOpenModelsDialogDoesNotBlockOnReload(t *testing.T) {
	ws := &reloadWorkspaceStub{cfg: newReloadTestConfig()}
	m := newReloadTestModels(t, ws)

	require.Zero(t, ws.reloadCalls,
		"constructing the dialog must not reload config synchronously")
	require.False(t, m.loading, "the dialog must not start in a loading state")
}

// TestModelsStartLoadingRefreshesInPlace verifies the background reload
// path end to end: StartLoading kicks off the check without blocking, and
// once the result lands (via HandleMsg), the dialog stops showing the
// loading indicator. A second StartLoading call while one is already in
// flight must be a no-op so repeated dialog opens can't stack fetches.
func TestModelsStartLoadingRefreshesInPlace(t *testing.T) {
	ws := &reloadWorkspaceStub{cfg: newReloadTestConfig(), reloadResult: true}
	m := newReloadTestModels(t, ws)

	cmd := m.StartLoading()
	require.NotNil(t, cmd, "StartLoading must return a command to run in the background")
	require.True(t, m.loading)

	require.Nil(t, m.StartLoading(), "a second StartLoading while one is in flight must be a no-op")
	require.Zero(t, ws.reloadCalls, "the reload command hasn't run yet -- only invoking cmd() triggers it")

	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	require.True(t, ok, "StartLoading batches the spinner tick with the reload check")
	require.NotEmpty(t, batch)

	// Find the reload result among the batched commands (order isn't
	// guaranteed to put it first).
	var reloaded *modelsConfigReloadedMsg
	for _, sub := range batch {
		if r, ok := sub().(modelsConfigReloadedMsg); ok {
			reloaded = &r
		}
	}
	require.NotNil(t, reloaded, "one of the batched commands must be the reload check")
	require.Equal(t, 1, ws.reloadCalls)

	action := m.HandleMsg(*reloaded)
	require.Nil(t, action)
	require.False(t, m.loading, "the loading indicator must clear once the result lands")
}
