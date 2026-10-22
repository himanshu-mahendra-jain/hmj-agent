package tools

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"hmj-agent/internal/chat"
)

type RunCommandTool struct {
	// TimeoutSeconds overrides the default command timeout (mainly for tests). 0 = default.
	TimeoutSeconds  int
	TimeoutDuration time.Duration
}

func (t *RunCommandTool) Name() string { return "run_command" }

func (t *RunCommandTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name:        "run_command",
			Description: "Run any shell command on the host (the agent runs on a dedicated machine with no restrictions), in the current working directory by default.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The command line string to execute.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional execution timeout in seconds (default 60, max 300).",
					},
					"workdir": map[string]any{
						"type":        "string",
						"description": "Optional directory to run the command in: relative to the current working directory, or absolute (~ is expanded). Must exist. Defaults to the current working directory.",
					},
				},
				"required":             []string{"command"},
				"additionalProperties": false,
			},
		},
	}
}

type runCommandInput struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Workdir        string `json:"workdir"`
}

type multiCappedWriter struct {
	mu        sync.Mutex
	limit     int
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	truncated bool
}

func newMultiCappedWriter(limit int) *multiCappedWriter {
	return &multiCappedWriter{limit: limit}
}

type streamWriter struct {
	mc       *multiCappedWriter
	isStderr bool
}

func (sw *streamWriter) Write(p []byte) (int, error) {
	sw.mc.mu.Lock()
	defer sw.mc.mu.Unlock()
	n := len(p)
	currentTotal := sw.mc.stdout.Len() + sw.mc.stderr.Len()
	buf := &sw.mc.stdout
	if sw.isStderr {
		buf = &sw.mc.stderr
	}

	if currentTotal < sw.mc.limit {
		remaining := sw.mc.limit - currentTotal
		if len(p) > remaining {
			buf.Write(p[:remaining])
			sw.mc.truncated = true
		} else {
			buf.Write(p)
		}
	} else {
		sw.mc.truncated = true
	}
	return n, nil
}

func (mc *multiCappedWriter) Formatted() (string, bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	stdoutStr := strings.TrimRight(mc.stdout.String(), "\r\n")
	stderrStr := strings.TrimRight(mc.stderr.String(), "\r\n")

	if stderrStr == "" {
		return stdoutStr, mc.truncated
	}
	if stdoutStr == "" {
		return stderrStr, mc.truncated
	}
	return fmt.Sprintf("[stdout]\n%s\n\n[stderr]\n%s", stdoutStr, stderrStr), mc.truncated
}

// parseRunCommandArgs decodes the tool arguments and requires a non-empty command.
func parseRunCommandArgs(argsJSON string) (runCommandInput, error) {
	var in runCommandInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return in, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return in, errors.New("command is required")
	}
	return in, nil
}

// resolveRunCommandWorkdir resolves the requested workdir and checks that it is an
// existing directory.
func resolveRunCommandWorkdir(requested string) (string, error) {
	workdir, err := ResolvePath(requested)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(workdir)
	if err != nil {
		return "", fmt.Errorf("invalid workdir %q: %w", requested, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workdir %q is not a directory", requested)
	}
	return workdir, nil
}

// timeoutFor picks the command timeout: the tool's configured default, overridden by
// the per-call timeout_seconds (capped at 300 seconds).
func (t *RunCommandTool) timeoutFor(in runCommandInput) time.Duration {
	timeout := 60 * time.Second
	if t.TimeoutDuration > 0 {
		timeout = t.TimeoutDuration
	} else if t.TimeoutSeconds > 0 {
		timeout = time.Duration(t.TimeoutSeconds) * time.Second
	}
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(min(in.TimeoutSeconds, 300)) * time.Second
	}
	return timeout
}

// newRunCommand builds the shell command for cmdStr in workdir, bound to ctx. On
// timeout the whole process group is killed, and WaitDelay bounds pipe drains.
func newRunCommand(ctx context.Context, cmdStr, workdir string, timeout time.Duration) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = workdir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// If the timeout fires, kill the entire process group hard; WaitDelay bounds how
	// long we wait on pipe drains so stubborn grandchild processes cannot hang.
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 1 {
			errKillGroup := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			errKillProc := cmd.Process.Kill()
			if errKillGroup != nil && !errors.Is(errKillGroup, syscall.ESRCH) {
				return errors.Join(errKillGroup, errKillProc)
			}
			return errKillProc
		}
		return nil
	}
	waitDelay := 1 * time.Second
	if timeout < 2*time.Second {
		waitDelay = 100 * time.Millisecond
	}
	cmd.WaitDelay = waitDelay
	return cmd
}

// runCommandExitNotice describes a failed command run, such as "[Exit code 2]".
func runCommandExitNotice(runErr error) string {
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		code := exitErr.ExitCode()
		// "exit status N" is just the ExitError's own text; repeating it
		// after the code wastes tokens. Keep both only when they differ.
		if runErr.Error() == "exit status "+strconv.Itoa(code) {
			return fmt.Sprintf("[Exit code %d]", code)
		}
		return fmt.Sprintf("[Exit code %d: %v]", code, runErr)
	}
	return fmt.Sprintf("[%v]", runErr)
}

// withNotice appends notice on its own line after res, or returns notice alone when
// res is empty.
func withNotice(res, notice string) string {
	if res == "" {
		return notice
	}
	return fmt.Sprintf("%s\n%s", res, notice)
}

func (t *RunCommandTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	in, err := parseRunCommandArgs(argsJSON)
	if err != nil {
		return "", err
	}
	cmdStr := strings.TrimSpace(in.Command)

	workdir, err := resolveRunCommandWorkdir(in.Workdir)
	if err != nil {
		return "", err
	}

	timeout := t.timeoutFor(in)
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newRunCommand(cmdCtx, cmdStr, workdir, timeout)

	mc := newMultiCappedWriter(MaxCommandOutputBytes)
	cmd.Stdout = &streamWriter{mc: mc, isStderr: false}
	cmd.Stderr = &streamWriter{mc: mc, isStderr: true}

	runErr := cmd.Run()

	res, truncated := mc.Formatted()
	if truncated {
		res += "\n[output truncated to 64KB]"
	}

	if errors.Is(cmdCtx.Err(), context.Canceled) {
		return withNotice(res, "[command cancelled by user]"), nil
	}
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return withNotice(res, "[command timed out]"), nil
	}

	if runErr != nil {
		return withNotice(res, runCommandExitNotice(runErr)), nil
	}

	if res == "" {
		return "[command executed successfully with no output]", nil
	}
	return res, nil
}
