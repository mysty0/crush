package compressd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// maxFrameBytes bounds a single framed message so a misbehaving or
// malicious peer can never make the client allocate an unbounded buffer.
const maxFrameBytes = 64 << 20 // 64MiB

// defaultDialTimeout bounds how long connecting to the daemon's Unix
// socket may take before Client gives up.
const defaultDialTimeout = 2 * time.Second

// Client talks to a running headroomd daemon over its Unix domain socket
// using a 4-byte big-endian length prefix followed by that many bytes of
// UTF-8 JSON, for both the request and the response. See the package
// doc comment for the full wire protocol.
type Client struct {
	socketPath  string
	dialTimeout time.Duration
}

// NewClient returns a Client that connects to the daemon listening on
// socketPath. It does not dial until a method is called.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath:  socketPath,
		dialTimeout: defaultDialTimeout,
	}
}

// request is the wire request envelope. Method is either "compress" or
// "ping"; Text and Threshold are only meaningful for "compress".
type request struct {
	Method    string  `json:"method"`
	Text      string  `json:"text,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

// response is the wire response envelope covering both compress and
// ping replies, plus the error shape.
type response struct {
	OK           bool     `json:"ok"`
	Compressed   string   `json:"compressed,omitempty"`
	KeepRate     float64  `json:"keep_rate,omitempty"`
	DroppedSpans [][2]int `json:"dropped_spans,omitempty"`
	ModelLoaded  bool     `json:"model_loaded,omitempty"`
	UptimeSecs   int64    `json:"uptime_secs,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// Compress asks the daemon to compress text, targeting the given
// keep-rate threshold (0-1), and returns the compressed text, the
// achieved keep rate, and the byte-offset spans (in the original text)
// that were dropped.
func (c *Client) Compress(ctx context.Context, text string, threshold float64) (compressed string, keepRate float64, droppedSpans [][2]int, err error) {
	resp, err := c.do(ctx, request{Method: "compress", Text: text, Threshold: threshold})
	if err != nil {
		return "", 0, nil, err
	}
	return resp.Compressed, resp.KeepRate, resp.DroppedSpans, nil
}

// Ping checks whether the daemon is reachable and responsive. It returns
// nil on a successful "ok" reply, regardless of whether the compression
// model has finished loading.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, request{Method: "ping"})
	return err
}

// do dials the daemon's socket, sends req, reads and decodes the
// response, and translates an "ok": false reply into an error. A fresh
// connection is opened per call: the protocol is simple request/response
// and daemon restarts (e.g. after a crash) must not leave the client
// stuck on a dead connection.
func (c *Client) do(ctx context.Context, req request) (response, error) {
	dialer := net.Dialer{Timeout: c.dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return response{}, fmt.Errorf("dial headroomd socket: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return response{}, fmt.Errorf("encode headroomd request: %w", err)
	}
	if err := writeFrame(conn, payload); err != nil {
		return response{}, fmt.Errorf("write headroomd request: %w", err)
	}

	respBytes, err := readFrame(conn)
	if err != nil {
		return response{}, fmt.Errorf("read headroomd response: %w", err)
	}
	var resp response
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return response{}, fmt.Errorf("decode headroomd response: %w", err)
	}
	if !resp.OK {
		return response{}, fmt.Errorf("headroomd returned an error: %s", resp.Error)
	}
	return resp, nil
}

// writeFrame writes a 4-byte big-endian length prefix followed by
// payload.
func writeFrame(w io.Writer, payload []byte) error {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readFrame reads a 4-byte big-endian length prefix followed by that
// many bytes, rejecting frames larger than maxFrameBytes.
func readFrame(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > maxFrameBytes {
		return nil, fmt.Errorf("frame too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
