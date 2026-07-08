package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/shell"
)

const (
	JobOutputToolName = "job_output"

	// DefaultJobOutputWaitSeconds bounds a wait=true call so a job that
	// never completes (a server, an interactive prompt, a hung process)
	// cannot block the agent forever.
	DefaultJobOutputWaitSeconds = 60
	MaxJobOutputWaitSeconds     = 600
)

//go:embed job_output.md
var jobOutputDescription string

type JobOutputParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to retrieve output from"`
	Wait    bool   `json:"wait" description:"If true, block until the background shell completes before returning output"`
	Timeout int    `json:"timeout,omitempty" description:"Maximum seconds to wait when wait=true (default: 60, max: 600). Returns the current status even if the job is still running."`
}

type JobOutputResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	WorkingDirectory string `json:"working_directory"`
}

func NewJobOutputTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobOutputToolName,
		jobOutputDescription,
		func(ctx context.Context, params JobOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			bgManager := shell.GetBackgroundShellManager()
			bgShell, ok := bgManager.Get(params.ShellID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell not found: %s", params.ShellID)), nil
			}

			waitSeconds := cmp.Or(params.Timeout, DefaultJobOutputWaitSeconds)
			if waitSeconds > MaxJobOutputWaitSeconds {
				waitSeconds = MaxJobOutputWaitSeconds
			}
			if params.Wait {
				// Bound the wait so a job that never finishes cannot block
				// the agent indefinitely. The call returns with the current
				// (possibly still-running) status once the timeout elapses.
				waitCtx, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds)*time.Second)
				bgShell.WaitContext(waitCtx)
				cancel()
			}

			stdout, stderr, done, err := bgShell.GetOutput()

			var outputParts []string
			if stdout != "" {
				outputParts = append(outputParts, stdout)
			}
			if stderr != "" {
				outputParts = append(outputParts, stderr)
			}

			status := "running"
			if done {
				status = "completed"
				if err != nil {
					exitCode := shell.ExitCode(err)
					if exitCode != 0 {
						outputParts = append(outputParts, fmt.Sprintf("Exit code %d", exitCode))
					}
				}
			} else if params.Wait {
				// The bounded wait elapsed and the job is still running.
				// Tell the agent so it stops polling blindly and decides
				// what to do next instead of getting stuck.
				outputParts = append(outputParts, fmt.Sprintf("Still running after waiting %ds. Use job_kill to stop it, or call job_output again to keep waiting.", waitSeconds))
			}

			output := strings.Join(outputParts, "\n")
			output = TruncateOutput(output)

			metadata := JobOutputResponseMetadata{
				ShellID:          params.ShellID,
				Command:          bgShell.Command,
				Description:      bgShell.Description,
				Done:             done,
				WorkingDirectory: bgShell.WorkingDir,
			}

			if output == "" {
				output = BashNoOutput
			}

			result := fmt.Sprintf("Status: %s\n\n%s", status, output)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}
