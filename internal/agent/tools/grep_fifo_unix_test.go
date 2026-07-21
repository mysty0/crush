//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestSearchFilesWithRegex_SkipsFIFO ensures the walker skips non-regular
// files. A FIFO (named pipe) under the search root would otherwise be opened
// and read by isTextFile, blocking forever waiting for a writer and wedging
// the whole grep — and, because a raw Read cannot observe context
// cancellation, the entire turn.
func TestSearchFilesWithRegex_SkipsFIFO(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	require.NoError(t, os.WriteFile(
		filepath.Join(tempDir, "real.txt"), []byte("hello world"), 0o644,
	))

	// A FIFO with no writer: opening it for read and calling Read blocks
	// indefinitely unless the walker skips it.
	require.NoError(t, unix.Mkfifo(filepath.Join(tempDir, "pipe"), 0o644))

	done := make(chan struct{})
	go func() {
		defer close(done)
		matches, err := searchFilesWithRegex(context.Background(), "hello world", tempDir, "")
		require.NoError(t, err)
		require.Len(t, matches, 1)
		require.Equal(t, "real.txt", filepath.Base(matches[0].path))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("searchFilesWithRegex hung on a FIFO instead of skipping it")
	}
}
