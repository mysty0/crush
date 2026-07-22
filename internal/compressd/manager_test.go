package compressd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUnavailableBackoff(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)
	now := base

	m := &Manager{
		unavailableAt: make(map[string]time.Time),
		now:           func() time.Time { return now },
	}

	require.False(t, m.recentlyUnavailable(unavailableKey))

	m.markUnavailable(unavailableKey)
	require.True(t, m.recentlyUnavailable(unavailableKey))

	now = now.Add(unavailableRetryDelay + time.Second)
	require.False(t, m.recentlyUnavailable(unavailableKey))
	_, exists := m.unavailableAt[unavailableKey]
	require.False(t, exists)

	m.markUnavailable(unavailableKey)
	m.clearUnavailable(unavailableKey)
	require.False(t, m.recentlyUnavailable(unavailableKey))
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	store, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	m := NewManager(store)
	return m
}

func TestResolveBinary_NotOnPath(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	m.lookPath = func(string) (string, error) {
		return "", os.ErrNotExist
	}

	_, err := m.resolveBinary()
	require.ErrorIs(t, err, errBinaryNotFound)
}

func TestResolveBinary_ConfiguredPathMissing(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	m.cfg.Config().Options.HeadroomBinaryPath = filepath.Join(t.TempDir(), "no-such-binary")

	_, err := m.resolveBinary()
	require.ErrorIs(t, err, errBinaryNotFound)
}

func TestResolveBinary_ConfiguredPathExists(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	binPath := filepath.Join(t.TempDir(), "headroomd")
	require.NoError(t, os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755))
	m.cfg.Config().Options.HeadroomBinaryPath = binPath

	got, err := m.resolveBinary()
	require.NoError(t, err)
	require.Equal(t, binPath, got)
}

func TestModelPaths_Unconfigured(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	_, _, err := m.modelPaths()
	require.ErrorIs(t, err, errNotConfigured)
}

func TestModelPaths_Configured(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	m.cfg.Config().Options.HeadroomModelPath = "/models/model.bin"
	m.cfg.Config().Options.HeadroomTokenizerPath = "/models/tokenizer.json"

	model, tok, err := m.modelPaths()
	require.NoError(t, err)
	require.Equal(t, "/models/model.bin", model)
	require.Equal(t, "/models/tokenizer.json", tok)
}

func TestBuildDaemonArgs_CPUDefault(t *testing.T) {
	t.Parallel()

	opts := &config.Options{}
	args := buildDaemonArgs("/sock", "/model.onnx", "/tok.json", opts)
	require.Equal(t, []string{
		"--socket", "/sock",
		"--model", "/model.onnx",
		"--tokenizer", "/tok.json",
	}, args)
}

func TestBuildDaemonArgs_GPUEnabled(t *testing.T) {
	t.Parallel()

	opts := &config.Options{
		HeadroomGPU:         true,
		HeadroomGPUDeviceID: 1,
	}
	args := buildDaemonArgs("/sock", "/model.onnx", "/tok.json", opts)
	require.Equal(t, []string{
		"--socket", "/sock",
		"--model", "/model.onnx",
		"--tokenizer", "/tok.json",
		"--gpu", "--gpu-device-id", "1",
	}, args)
}

func TestBuildDaemonArgs_CustomOrtDylibPath(t *testing.T) {
	t.Parallel()

	opts := &config.Options{
		HeadroomOrtDylibPath: "/custom/libonnxruntime.so",
	}
	args := buildDaemonArgs("/sock", "/model.onnx", "/tok.json", opts)
	require.Equal(t, []string{
		"--socket", "/sock",
		"--model", "/model.onnx",
		"--tokenizer", "/tok.json",
		"--ort-dylib-path", "/custom/libonnxruntime.so",
	}, args)
}

func TestBuildDaemonArgs_GPUAndCustomOrtDylibPath(t *testing.T) {
	t.Parallel()

	opts := &config.Options{
		HeadroomGPU:          true,
		HeadroomGPUDeviceID:  0,
		HeadroomOrtDylibPath: "/custom/libonnxruntime.so",
	}
	args := buildDaemonArgs("/sock", "/model.onnx", "/tok.json", opts)
	require.Equal(t, []string{
		"--socket", "/sock",
		"--model", "/model.onnx",
		"--tokenizer", "/tok.json",
		"--gpu", "--gpu-device-id", "0",
		"--ort-dylib-path", "/custom/libonnxruntime.so",
	}, args)
}

// TestClient_DisabledByConfig verifies that an explicit false disables
// compression without attempting to start the daemon at all.
func TestClient_DisabledByConfig(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	disabled := false
	m.cfg.Config().Options.CompressToolOutputs = &disabled

	client, ok := m.Client(t.Context())
	require.False(t, ok)
	require.Nil(t, client)
}

// TestClient_UnavailableWhenBinaryMissing verifies the graceful-disable
// path: no binary on PATH means Client reports unavailable rather than
// erroring, and the failure is then backed off.
func TestClient_UnavailableWhenBinaryMissing(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	m.lookPath = func(string) (string, error) {
		return "", os.ErrNotExist
	}

	client, ok := m.Client(t.Context())
	require.False(t, ok)
	require.Nil(t, client)
	require.Equal(t, StateDisabled, m.State())
	require.True(t, m.recentlyUnavailable(unavailableKey))
}

func TestDefaultSocketPath_UsesRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	require.Equal(t, "/run/user/1000/crush/headroomd.sock", defaultSocketPath())
}

func TestDefaultSocketPath_FallsBackToTmp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	got := defaultSocketPath()
	require.Contains(t, got, "/tmp/crush-headroomd-")
}

func TestClose_NoProcess(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	require.NoError(t, m.Close(t.Context()))
	require.Equal(t, StateStopped, m.State())
}
