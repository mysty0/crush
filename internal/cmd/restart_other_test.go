//go:build !windows

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveRestartExecutable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"normal path is untouched", "/usr/local/bin/crush", "/usr/local/bin/crush"},
		{"deleted suffix is stripped", "/usr/local/bin/crush (deleted)", "/usr/local/bin/crush"},
		{"a path that merely contains the word deleted is untouched", "/home/user/deleted/crush", "/home/user/deleted/crush"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, resolveRestartExecutable(tt.in))
		})
	}
}

func TestBuildRestartArgs(t *testing.T) {
	t.Parallel()

	t.Run("no flags set produces just argv0", func(t *testing.T) {
		t.Parallel()
		got := buildRestartArgs("/bin/crush", restartArgs{})
		assert.Equal(t, []string{"/bin/crush"}, got)
	})

	t.Run("every flag round-trips when set", func(t *testing.T) {
		t.Parallel()
		got := buildRestartArgs("/bin/crush", restartArgs{
			Debug:     true,
			Host:      "unix:///run/user/1000/crush.sock",
			DataDir:   "/custom/data",
			Yolo:      true,
			SessionID: "abc123",
		})
		assert.Equal(t, []string{
			"/bin/crush",
			"--debug",
			"--host", "unix:///run/user/1000/crush.sock",
			"--data-dir", "/custom/data",
			"--yolo",
			"--session", "abc123",
		}, got)
	})

	t.Run("session id is only passed when non-empty", func(t *testing.T) {
		t.Parallel()
		got := buildRestartArgs("/bin/crush", restartArgs{SessionID: ""})
		assert.NotContains(t, got, "--session")
	})
}
