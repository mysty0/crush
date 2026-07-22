// Package compressd supervises the headroomd compression daemon (a
// separate Rust binary living in compressd/ at the repo root) and
// provides a client for its Unix-socket IPC protocol.
//
// The Manager follows the same lazy-start, health-tracking,
// unavailable-backoff shape as [github.com/charmbracelet/crush/internal/lsp.Manager]:
// the daemon is not spawned at app startup, only the first time
// compression is actually needed, and a daemon that fails to start (missing
// binary, missing model/tokenizer configuration) is marked unavailable for
// a backoff window instead of being retried on every call.
package compressd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/config"
)

// unavailableRetryDelay bounds how often Manager re-attempts starting the
// daemon after a failed attempt (missing binary, missing model files, a
// crash on startup, etc). Mirrors [lsp.unavailableRetryDelay].
const unavailableRetryDelay = 30 * time.Second

// daemonBinaryName is the executable Manager looks for on PATH when no
// explicit config.Options.HeadroomBinaryPath is set.
const daemonBinaryName = "headroomd"

// startTimeout bounds how long Manager waits for a freshly spawned daemon
// to answer a ping before giving up and marking it unavailable.
const startTimeout = 10 * time.Second

// unavailableKey is the single key used in the unavailable map. There is
// only ever one daemon instance per Manager, but a map (rather than a bare
// field) is used to mirror lsp.Manager's recentlyUnavailable/
// markUnavailable/clearUnavailable shape as closely as possible.
const unavailableKey = "headroomd"

// State represents the health of the supervised daemon process.
type State int

const (
	// StateStopped means no daemon process is currently running.
	StateStopped State = iota
	// StateStarting means the daemon process has been spawned and Manager
	// is waiting for it to become responsive.
	StateStarting
	// StateReady means the daemon answered a ping and is ready to serve
	// compress requests.
	StateReady
	// StateError means the most recent start attempt failed.
	StateError
	// StateDisabled means compression is turned off by config, or the
	// binary/model/tokenizer are not available, so Manager will not
	// attempt to start the daemon at all.
	StateDisabled
)

// Manager lazily starts and supervises the headroomd subprocess and hands
// out a [Client] connected to it. It is safe for concurrent use.
type Manager struct {
	cfg *config.ConfigStore

	mu     sync.Mutex
	cmd    *exec.Cmd
	client *Client

	state      atomic.Value // State
	socketPath string

	// unavailableAt tracks, per unavailableKey, when the daemon was last
	// found unavailable so retries are backed off. See recentlyUnavailable.
	unavailableAt map[string]time.Time
	unavailMu     sync.Mutex
	now           func() time.Time

	// lookPath and statFile are overridable for tests.
	lookPath func(string) (string, error)
	statFile func(string) (os.FileInfo, error)
}

// NewManager creates a new compressd Manager. It does not start any
// process; call Client to lazily start the daemon the first time it is
// needed.
func NewManager(cfg *config.ConfigStore) *Manager {
	m := &Manager{
		cfg:           cfg,
		unavailableAt: make(map[string]time.Time),
		now:           time.Now,
		lookPath:      exec.LookPath,
		statFile:      os.Stat,
		socketPath:    defaultSocketPath(),
	}
	m.state.Store(StateStopped)
	return m
}

// defaultSocketPath returns $XDG_RUNTIME_DIR/crush/headroomd.sock when
// XDG_RUNTIME_DIR is set, otherwise /tmp/crush-headroomd-<uid>.sock.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "crush", "headroomd.sock")
	}
	return fmt.Sprintf("/tmp/crush-headroomd-%d.sock", os.Getuid())
}

// State returns the current health state of the supervised daemon.
func (m *Manager) State() State {
	if v := m.state.Load(); v != nil {
		return v.(State)
	}
	return StateStopped
}

// Enabled reports whether tool-output compression is turned on by config.
// Nil (unset) defaults to enabled, matching the AutoLSP/AutoDiscoverModels
// tri-state convention used elsewhere in config.Options.
func (m *Manager) Enabled() bool {
	opts := m.cfg.Config().Options
	return opts.CompressToolOutputs == nil || *opts.CompressToolOutputs
}

// Client returns a Client connected to a ready daemon, lazily starting the
// subprocess on first use. It returns ok=false whenever compression is
// disabled by config, the daemon is not available (missing binary or
// model/tokenizer configuration), or a recent start attempt failed and the
// backoff window has not yet elapsed. Callers must treat ok=false as "skip
// compression for now" and never fail the caller's operation because of
// it.
func (m *Manager) Client(ctx context.Context) (*Client, bool) {
	if !m.Enabled() {
		return nil, false
	}
	if m.recentlyUnavailable(unavailableKey) {
		return nil, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.client != nil && m.State() == StateReady {
		return m.client, true
	}

	if err := m.startLocked(ctx); err != nil {
		slog.Debug("headroomd unavailable; skipping tool-output compression", "error", err)
		m.markUnavailable(unavailableKey)
		return nil, false
	}
	m.clearUnavailable(unavailableKey)
	return m.client, true
}

// errBinaryNotFound and errNotConfigured are returned by startLocked to
// distinguish "will never work without user action" cases in logs; callers
// only care that compression is unavailable, not why.
var (
	errBinaryNotFound = errors.New("headroomd binary not found")
	errNotConfigured  = errors.New("headroomd model/tokenizer paths not configured")
)

// resolveBinary returns the path to the headroomd binary: the configured
// override if set, otherwise the first match on PATH.
//
// TODO(headroomd): when no binary is found and none is configured, a
// future version should offer to download a matching headroomd release
// with explicit user consent (see the design doc). This v1 only
// supervises a binary that already exists; it never downloads one.
func (m *Manager) resolveBinary() (string, error) {
	if p := m.cfg.Config().Options.HeadroomBinaryPath; p != "" {
		if _, err := m.statFile(p); err != nil {
			return "", fmt.Errorf("%w: configured path %q: %w", errBinaryNotFound, p, err)
		}
		return p, nil
	}
	p, err := m.lookPath(daemonBinaryName)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errBinaryNotFound, err)
	}
	return p, nil
}

// modelPaths returns the configured model and tokenizer paths, or an error
// if either is unset.
//
// TODO(headroomd): a future version should offer to auto-download a
// default model/tokenizer pair with explicit user consent (see the design
// doc) when these are unset. This v1 only supports paths that already
// exist on disk.
func (m *Manager) modelPaths() (modelPath, tokenizerPath string, err error) {
	opts := m.cfg.Config().Options
	if opts.HeadroomModelPath == "" || opts.HeadroomTokenizerPath == "" {
		return "", "", errNotConfigured
	}
	return opts.HeadroomModelPath, opts.HeadroomTokenizerPath, nil
}

// buildDaemonArgs builds the headroomd CLI argument list for the given
// socket/model/tokenizer paths, adding --gpu/--gpu-device-id and
// --ort-dylib-path when configured. Split out from startLocked so the
// argument-building logic is testable without spawning a process.
func buildDaemonArgs(socketPath, modelPath, tokenizerPath string, opts *config.Options) []string {
	args := []string{
		"--socket", socketPath,
		"--model", modelPath,
		"--tokenizer", tokenizerPath,
	}
	if opts.HeadroomGPU {
		args = append(args, "--gpu", "--gpu-device-id", strconv.Itoa(opts.HeadroomGPUDeviceID))
	}
	if opts.HeadroomOrtDylibPath != "" {
		args = append(args, "--ort-dylib-path", opts.HeadroomOrtDylibPath)
	}
	return args
}

// startLocked spawns the daemon and waits for it to become ready. Callers
// must hold m.mu.
func (m *Manager) startLocked(ctx context.Context) error {
	bin, err := m.resolveBinary()
	if err != nil {
		m.state.Store(StateDisabled)
		return err
	}
	modelPath, tokenizerPath, err := m.modelPaths()
	if err != nil {
		m.state.Store(StateDisabled)
		return err
	}

	if err := os.MkdirAll(filepath.Dir(m.socketPath), 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	// A stale socket file left behind by a killed daemon would otherwise
	// make the new process fail to bind.
	_ = os.Remove(m.socketPath)

	// Detach the process's context from ctx (typically a single request's
	// context): the daemon must keep running across many requests, not
	// just the one that happened to trigger the lazy start.
	args := buildDaemonArgs(m.socketPath, modelPath, tokenizerPath, m.cfg.Config().Options)
	cmd := exec.CommandContext(context.WithoutCancel(ctx), bin, args...)
	if err := cmd.Start(); err != nil {
		m.state.Store(StateError)
		return fmt.Errorf("start headroomd: %w", err)
	}

	m.cmd = cmd
	m.state.Store(StateStarting)

	// Reap the process and reset state if it exits on its own (crash,
	// killed externally, etc) so the next Client call retries a fresh
	// start rather than reusing a dead client.
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		if m.cmd == cmd {
			m.cmd = nil
			m.client = nil
			m.state.Store(StateStopped)
		}
		m.mu.Unlock()
	}()

	client := NewClient(m.socketPath)
	if err := waitReady(ctx, client, startTimeout); err != nil {
		_ = cmd.Process.Kill()
		m.state.Store(StateError)
		return fmt.Errorf("headroomd did not become ready: %w", err)
	}

	m.client = client
	m.state.Store(StateReady)
	slog.Debug("headroomd started", "socket", m.socketPath)
	return nil
}

// waitReady polls Ping until it succeeds or timeout elapses.
func waitReady(ctx context.Context, client *Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const pollInterval = 50 * time.Millisecond
	var lastErr error
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, pollInterval)
		lastErr = client.Ping(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf("timed out waiting for headroomd: %w", lastErr)
}

// recentlyUnavailable reports whether key was marked unavailable within
// unavailableRetryDelay. Mirrors lsp.Manager.recentlyUnavailable.
func (m *Manager) recentlyUnavailable(key string) bool {
	m.unavailMu.Lock()
	defer m.unavailMu.Unlock()
	lastUnavailableAt, exists := m.unavailableAt[key]
	if !exists {
		return false
	}
	if m.now().Sub(lastUnavailableAt) < unavailableRetryDelay {
		return true
	}
	delete(m.unavailableAt, key)
	return false
}

// markUnavailable records that key just failed to become available.
func (m *Manager) markUnavailable(key string) {
	m.unavailMu.Lock()
	m.unavailableAt[key] = m.now()
	m.unavailMu.Unlock()
}

// clearUnavailable removes any backoff record for key.
func (m *Manager) clearUnavailable(key string) {
	m.unavailMu.Lock()
	delete(m.unavailableAt, key)
	m.unavailMu.Unlock()
}

// Close kills the supervised daemon process, if running. Safe to call
// even if the daemon was never started. Intended for app shutdown.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil {
		m.state.Store(StateStopped)
		return nil
	}
	err := m.cmd.Process.Kill()
	_ = m.cmd.Wait()
	m.cmd = nil
	m.client = nil
	m.state.Store(StateStopped)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill headroomd: %w", err)
	}
	return nil
}
