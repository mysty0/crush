package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/compressd"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// The tests in this file guard one invariant: compressing the same tool
// output twice must produce byte-identical text.
//
// Why it matters: tool-output compression is deliberately ephemeral. It
// rewrites only the copy of the message slice handed to the provider for
// the current turn, never the persisted message (see the
// compressPriorToolResults call in PrepareStep). So every turn rebuilds
// the provider messages from the same persisted, uncompressed text and
// re-compresses it from scratch. Providers with prompt caching (Anthropic
// in particular) key on an exact byte prefix of the request: if turn N+1
// renders a prior tool result even one byte differently from turn N, the
// cache prefix diverges at that message and everything after it -- the
// entire rest of the conversation -- is a cache miss, on every single
// turn. A conversation whose history is stable caches at 75-99%; one that
// re-randomizes a prior message caches only the static system+tools
// prefix.
//
// These tests exercise the real production path (the closure built by
// buildCompressToolOutput, driving a real compressd.Manager over a real
// Unix socket) so the instability they catch is the one that actually
// ships, not a reimplementation of it.

// fakeHeadroomdEnv marks a re-exec of the test binary as "run as the fake
// headroomd daemon" rather than as a normal test run.
const fakeHeadroomdEnv = "CRUSH_FAKE_HEADROOMD"

// retrieveIDPattern extracts the retrieve_full_output id embedded in a
// compressed replacement, so a failure points at the unstable id rather
// than dumping two multi-kilobyte blobs.
var retrieveIDPattern = regexp.MustCompile(`id="([^"]+)"`)

// TestBuildCompressToolOutput_StableAcrossTurns compresses identical
// content twice through the production closure and requires the two
// results to be byte-identical, because the provider prompt cache keys on
// exact request-prefix bytes.
func TestBuildCompressToolOutput_StableAcrossTurns(t *testing.T) {
	// No t.Parallel: this test sets XDG_RUNTIME_DIR (via
	// newFakeDaemonCoordinator) to keep the daemon socket hermetic, and
	// t.Setenv is incompatible with parallel tests.
	c := newFakeDaemonCoordinator(t)

	fn := c.buildCompressToolOutput(false)
	require.NotNil(t, fn)

	content := longText(compressToolResultThresholdBytes + 500)

	first, ok := fn(t.Context(), "session-1", content)
	require.True(t, ok, "fake headroomd should be reachable")
	second, ok := fn(t.Context(), "session-1", content)
	require.True(t, ok, "fake headroomd should be reachable")

	firstID := extractRetrieveID(t, first)
	secondID := extractRetrieveID(t, second)

	// The id is part of the text sent to the model, so a fresh id per
	// call is by itself enough to break the cache prefix.
	require.Equal(t, firstID, secondID,
		"compressing identical content must reuse the same retrieval id; "+
			"a fresh id changes the bytes of an old message and invalidates "+
			"the provider prompt cache from that message onwards")
	require.Equal(t, first, second,
		"compressing identical content must be byte-identical across turns")

	// The compressed text must still resolve back to the original, so a
	// fix that stabilizes the id cannot do so by dropping the store
	// entry.
	original, found := c.retrieveStore.Get("session-1", firstID)
	require.True(t, found)
	require.Equal(t, content, original)
}

// TestCompressPriorToolResults_StableAcrossTurns drives the real
// per-turn entry point, compressPriorToolResults, over the same message
// history twice -- exactly what PrepareStep does on consecutive turns,
// since compression never touches the persisted messages -- and requires
// both turns to render the prior tool result identically.
func TestCompressPriorToolResults_StableAcrossTurns(t *testing.T) {
	// No t.Parallel: see TestBuildCompressToolOutput_StableAcrossTurns.
	c := newFakeDaemonCoordinator(t)

	fn := c.buildCompressToolOutput(false)
	require.NotNil(t, fn)

	// newTurn rebuilds the provider message slice from scratch, the way
	// PrepareStep does: the persisted tool result is still the original
	// uncompressed text on every turn, because compression only ever
	// mutated a previous turn's throwaway copy.
	newTurn := func() []fantasy.Message {
		return []fantasy.Message{
			fantasy.NewUserMessage("read the file and then grep it"),
			textToolResultMessage("call-1", longText(compressToolResultThresholdBytes+500)),
			assistantTextMessage("got it, grepping now"),
			textToolResultMessage("call-2", longText(compressToolResultThresholdBytes+700)),
		}
	}

	turnOne := newTurn()
	compressPriorToolResults(t.Context(), "session-1", turnOne, fn)

	turnTwo := newTurn()
	compressPriorToolResults(t.Context(), "session-1", turnTwo, fn)

	require.NotEqual(t, toolResultText(t, newTurn()[1]), toolResultText(t, turnOne[1]),
		"sanity: the prior-step tool result should actually have been compressed")
	require.Equal(t, toolResultText(t, turnOne[1]), toolResultText(t, turnTwo[1]),
		"the same conversation history must serialize to the same bytes on "+
			"every turn; otherwise the Anthropic prompt-cache prefix diverges "+
			"at the first compressed tool result and every later message is a "+
			"cache miss for the rest of the session")
}

// extractRetrieveID pulls the retrieve_full_output id out of a compressed
// replacement.
func extractRetrieveID(t *testing.T, replacement string) string {
	t.Helper()
	m := retrieveIDPattern.FindStringSubmatch(replacement)
	require.Len(t, m, 2, "replacement should embed a retrieve_full_output id: %q", replacement)
	return m[1]
}

// newFakeDaemonCoordinator builds a coordinator whose compressd.Manager
// supervises a fake headroomd instead of the real Rust binary, so the
// production path (closure -> Manager.Client -> compressd.Client -> Unix
// socket) runs end to end without needing the daemon installed.
//
// The Manager's socket path is unexported and resolved once in
// NewManager from XDG_RUNTIME_DIR, so overriding that environment
// variable is how a test redirects it without widening the compressd API.
// It also keeps the test off the developer's real headroomd socket, which
// Manager.startLocked would otherwise unlink.
func newFakeDaemonCoordinator(t *testing.T) *coordinator {
	t.Helper()

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	opts := cfg.Config().Options
	opts.HeadroomBinaryPath = writeFakeHeadroomdBinary(t)
	// Manager.modelPaths only requires these to be non-empty; the fake
	// daemon ignores them.
	opts.HeadroomModelPath = "fake-model"
	opts.HeadroomTokenizerPath = "fake-tokenizer"

	mgr := compressd.NewManager(cfg)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	return &coordinator{
		cfg:           cfg,
		compressdMgr:  mgr,
		retrieveStore: compressd.NewRetrievalStore(),
	}
}

// writeFakeHeadroomdBinary writes an executable stub that re-execs this
// test binary into TestFakeHeadroomdDaemon, forwarding the --socket flag
// Manager passes. Re-execing the test binary avoids compiling a separate
// helper program while still giving Manager a real child process to
// supervise, with the real start/ping/kill lifecycle.
func writeFakeHeadroomdBinary(t *testing.T) string {
	t.Helper()

	testBin, err := os.Executable()
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "fake-headroomd")
	// The "--" stops Go's flag parsing so headroomd's own flags reach
	// os.Args without the test binary rejecting them.
	script := fmt.Sprintf(
		"#!/bin/sh\nexport %s=1\nexec %q -test.run='^TestFakeHeadroomdDaemon$' -- \"$@\"\n",
		fakeHeadroomdEnv, testBin,
	)
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// TestFakeHeadroomdDaemon is not a test: it is the entry point the fake
// headroomd stub re-execs into. It serves the real length-prefixed JSON
// wire protocol on the socket it was given until the Manager kills it.
// Its compress replies are a pure function of the request text, so any
// instability the tests above observe originates in Crush, not here.
func TestFakeHeadroomdDaemon(t *testing.T) {
	if os.Getenv(fakeHeadroomdEnv) != "1" {
		t.Skip("only runs when re-execed as the fake headroomd daemon")
	}

	var socketPath string
	for i, arg := range os.Args {
		if arg == "--socket" && i+1 < len(os.Args) {
			socketPath = os.Args[i+1]
		}
	}
	require.NotEmpty(t, socketPath)

	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveFakeHeadroomd(conn)
	}
}

// serveFakeHeadroomd answers a single request on conn using headroomd's
// framing: a 4-byte big-endian length prefix followed by JSON.
func serveFakeHeadroomd(conn net.Conn) {
	defer conn.Close()

	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return
	}
	payload := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return
	}
	var req struct {
		Method string `json:"method"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return
	}

	var resp map[string]any
	switch req.Method {
	case "ping":
		resp = map[string]any{"ok": true, "model_loaded": true}
	case "compress":
		resp = map[string]any{
			"ok":         true,
			"compressed": fakeCompress(req.Text),
			"keep_rate":  0.5,
		}
	default:
		resp = map[string]any{"ok": false, "error": "unknown method"}
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(out)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return
	}
	_, _ = conn.Write(out)
}

// fakeCompress is deterministic on purpose: identical input must yield
// identical output, so the daemon can never be the source of the
// turn-to-turn byte drift these tests look for.
func fakeCompress(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("compressed(%d bytes, %x)", len(text), sum[:8])
}
