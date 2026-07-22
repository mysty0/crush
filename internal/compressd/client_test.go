package compressd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startMockDaemon spins up a Unix socket listener that behaves like
// headroomd for wire-protocol testing: it decodes the length-prefixed
// JSON request and calls handle to produce the length-prefixed JSON
// response. It stops when the test ends.
func startMockDaemon(t *testing.T, handle func(req map[string]any) response) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "headroomd.sock")
	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reqBytes, err := readFrame(conn)
				if err != nil {
					return
				}
				var req map[string]any
				if err := json.Unmarshal(reqBytes, &req); err != nil {
					return
				}
				resp := handle(req)
				payload, err := json.Marshal(resp)
				if err != nil {
					return
				}
				_ = writeFrame(conn, payload)
			}()
		}
	}()

	return socketPath
}

func TestClient_Compress_RoundTrip(t *testing.T) {
	t.Parallel()

	socketPath := startMockDaemon(t, func(req map[string]any) response {
		require.Equal(t, "compress", req["method"])
		require.Equal(t, "hello world", req["text"])
		require.InDelta(t, 0.5, req["threshold"], 0.0001)
		return response{
			OK:           true,
			Compressed:   "hello",
			KeepRate:     0.81,
			DroppedSpans: [][2]int{{5, 11}},
			ModelLoaded:  true,
		}
	})

	client := NewClient(socketPath)
	compressed, keepRate, spans, err := client.Compress(t.Context(), "hello world", 0.5)
	require.NoError(t, err)
	require.Equal(t, "hello", compressed)
	require.InDelta(t, 0.81, keepRate, 0.0001)
	require.Equal(t, [][2]int{{5, 11}}, spans)
}

func TestClient_Ping(t *testing.T) {
	t.Parallel()

	socketPath := startMockDaemon(t, func(req map[string]any) response {
		require.Equal(t, "ping", req["method"])
		return response{OK: true, ModelLoaded: true, UptimeSecs: 42}
	})

	client := NewClient(socketPath)
	require.NoError(t, client.Ping(t.Context()))
}

func TestClient_ErrorResponse(t *testing.T) {
	t.Parallel()

	socketPath := startMockDaemon(t, func(map[string]any) response {
		return response{OK: false, Error: "model not loaded"}
	})

	client := NewClient(socketPath)
	err := client.Ping(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "model not loaded")
}

func TestClient_NoDaemon(t *testing.T) {
	t.Parallel()

	client := NewClient(filepath.Join(t.TempDir(), "does-not-exist.sock"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := client.Ping(ctx)
	require.Error(t, err)
}

func TestFraming_RoundTrip(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	payload := []byte(`{"method":"ping"}`)
	go func() {
		_ = writeFrame(client, payload)
	}()

	got, err := readFrame(server)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestReadFrame_RejectsOversizedFrame(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], maxFrameBytes+1)
		_, _ = client.Write(lenBuf[:])
	}()

	_, err := readFrame(server)
	require.Error(t, err)
}
