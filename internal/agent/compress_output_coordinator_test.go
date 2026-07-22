package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/compressd"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestBuildCompressToolOutput_NilManager verifies no compression closure is
// built when the coordinator has no compressd.Manager wired up (e.g. a
// minimal test coordinator, or a build that never enables the feature).
func TestBuildCompressToolOutput_NilManager(t *testing.T) {
	t.Parallel()

	c := &coordinator{}
	require.Nil(t, c.buildCompressToolOutput(false))
}

// TestBuildCompressToolOutput_SubAgent verifies sub-agents never get a
// compression closure, even with a manager wired up.
func TestBuildCompressToolOutput_SubAgent(t *testing.T) {
	t.Parallel()

	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	c := &coordinator{cfg: cfg, compressdMgr: compressd.NewManager(cfg), retrieveStore: compressd.NewRetrievalStore()}

	require.Nil(t, c.buildCompressToolOutput(true))
}

// TestBuildCompressToolOutput_DisabledByConfig verifies an explicit
// compress_tool_outputs: false leaves tool-result content untouched
// without even attempting to reach the daemon.
func TestBuildCompressToolOutput_DisabledByConfig(t *testing.T) {
	t.Parallel()

	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	disabled := false
	cfg.Config().Options.CompressToolOutputs = &disabled

	c := &coordinator{cfg: cfg, compressdMgr: compressd.NewManager(cfg), retrieveStore: compressd.NewRetrievalStore()}
	fn := c.buildCompressToolOutput(false)
	require.NotNil(t, fn)

	replacement, ok := fn(t.Context(), "session-1", "some content")
	require.False(t, ok)
	require.Empty(t, replacement)
}

// TestBuildCompressToolOutput_DaemonUnavailable verifies that when the
// headroomd binary can't be found (the default in this sandboxed test
// environment, since no daemon is installed), the closure reports
// unavailable rather than erroring -- compression must never break a
// turn just because the daemon isn't there.
func TestBuildCompressToolOutput_DaemonUnavailable(t *testing.T) {
	t.Parallel()

	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	// Force a binary path that can never resolve, regardless of what's on
	// the host's PATH, so this test is hermetic.
	cfg.Config().Options.HeadroomBinaryPath = t.TempDir() + "/no-such-headroomd"

	c := &coordinator{cfg: cfg, compressdMgr: compressd.NewManager(cfg), retrieveStore: compressd.NewRetrievalStore()}
	fn := c.buildCompressToolOutput(false)
	require.NotNil(t, fn)

	replacement, ok := fn(t.Context(), "session-1", "some content")
	require.False(t, ok)
	require.Empty(t, replacement)
}
