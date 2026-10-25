package tools

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"hmj-agent/v2/internal/chat"
)

const (
	// maxCheckWaitSeconds caps check_command's wait_seconds. A model that waits longer holds
	// the prompt for nothing: it is told when a job ends without asking.
	maxCheckWaitSeconds = 120
	// checkedJobLines and checkedJobBytes bound the output check_command returns; the log
	// has the rest.
	checkedJobLines = 100
	checkedJobBytes = 16 * 1024
)

// CheckCommandTool reports on a background job started by run_command, or stops it.
type CheckCommandTool struct{}

func (t *CheckCommandTool) Name() string { return "check_command" }

func (t *CheckCommandTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name: "check_command",
			Description: "Report on a background job that run_command started (background: true) or that the user moved to the background: " +
				"whether it is running or how it ended, its exit code, how long it has run and its latest output lines, with the path of its full log. " +
				"You are told when a job ends without asking, so do not poll in a loop: end your turn instead. stop ends the job and its child processes.",
			Parameters: map[string]any{
				"type":             "object",
				chat.PropertyOrder: []string{"job", "wait_seconds", "stop"},
				"properties": map[string]any{
					"job": map[string]any{
						"type":        "integer",
						"description": "The job number run_command gave.",
					},
					"wait_seconds": map[string]any{
						"type":        "integer",
						"description": fmt.Sprintf("Optional: wait up to this many seconds (max %d) for the job to end before reporting. Default 0.", maxCheckWaitSeconds),
					},
					"stop": map[string]any{
						"type":        "boolean",
						"description": "Stop the job, killing its process group, then report on it.",
					},
				},
				"required":             []string{"job"},
				"additionalProperties": false,
			},
		},
	}
}

type checkCommandInput struct {
	Job         int  `json:"job"`
	WaitSeconds int  `json:"wait_seconds"`
	Stop        bool `json:"stop"`
}

func (t *CheckCommandTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var in checkCommandInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	owner := scratchDirFrom(ctx)
	j := Jobs.get(owner, in.Job)
	if j == nil {
		return "", Jobs.missing(owner, in.Job)
	}
	if in.Stop {
		if err := Jobs.Stop(owner, in.Job); err != nil {
			return "", err
		}
	} else if wait := min(in.WaitSeconds, maxCheckWaitSeconds); wait > 0 {
		timer := time.NewTimer(time.Duration(wait) * time.Second)
		defer timer.Stop()
		select {
		case <-j.proc.done:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	if j.done() {
		// The registry marks the end once the process is reaped; wait the moment that takes,
		// so a finished job reports finished.
		Jobs.waitEnded(j)
	}
	return Jobs.report(j, checkedJobLines, checkedJobBytes), nil
}
