package tools

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
			Name: "run_command",
			Description: "Run any shell command on the host (the agent runs on a dedicated machine with no restrictions), in the current working directory by default. " +
				"It runs under sh with pipefail set where sh supports it, so a pipeline such as `make | tail` fails, and $? reports, when any command in it fails. " +
				hostShellNote() +
				"grep, rg, diff and cmp also exit 1 when they match nothing or find a difference, so a filter such as `| grep -v '^w:'` that leaves no lines fails the pipeline: " +
				"to check a build's own status, redirect its output to a file instead of filtering it. " +
				"stdout and stderr come back together, in the order the command wrote them. An output over 64KB is returned as its first 16KB and its last 48KB, " +
				"and the whole output is saved to a file the result names, for read_file or grep. " +
				"Set background for a command likely to take more than about 2 minutes, such as a full test suite, a benchmark or an emulator boot: " +
				"it returns at once with a job number, and its output goes to a log. Then tell the user what is running and end your turn rather than waiting: " +
				"you are told when it ends, with its exit code and last lines, in a later tool result or message, and the user can talk to you meanwhile. " +
				"check_command reports on a job or stops it. The user can also move a running command to the background with Ctrl+B.",
			Parameters: map[string]any{
				"type":             "object",
				chat.PropertyOrder: []string{"command", "background", "timeout_seconds", "workdir"},
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The command line string to execute.",
					},
					"background": map[string]any{
						"type":        "boolean",
						"description": "Run the command as a background job: the call returns at once with its job number, and you are told when it ends.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional execution timeout in seconds: in the foreground default 60, max 1800; in the background default and max 14400 (4 hours).",
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
	Background     bool   `json:"background"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Workdir        string `json:"workdir"`
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
// the per-call timeout_seconds (capped at MaxCommandTimeoutSeconds).
func (t *RunCommandTool) timeoutFor(in runCommandInput) time.Duration {
	timeout := 60 * time.Second
	if t.TimeoutDuration > 0 {
		timeout = t.TimeoutDuration
	} else if t.TimeoutSeconds > 0 {
		timeout = time.Duration(t.TimeoutSeconds) * time.Second
	}
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(min(in.TimeoutSeconds, MaxCommandTimeoutSeconds)) * time.Second
	}
	return timeout
}

// hostShellNote names the shell sh is on this host when it is not bash, whose syntax a model
// tends to write: dash, for one, reads $'\t' as $\t, not a tab, and fails on [[ ]].
var hostShellNote = sync.OnceValue(func() string {
	sh, err := exec.LookPath("sh")
	if err != nil {
		return ""
	}
	if target, err := filepath.EvalSymlinks(sh); err == nil {
		sh = target
	}
	_, bashErr := exec.LookPath("bash")
	return shellNote(filepath.Base(sh), bashErr == nil)
})

// shellNote is the description's sentence on a host whose sh is name, or "" for bash.
func shellNote(name string, hasBash bool) string {
	if name == "" || name == "bash" || name == "sh" {
		return ""
	}
	note := "Here sh is " + name + ", which lacks bash-only syntax such as $'\\t', [[ ]], arrays and <(…)"
	if hasBash {
		return note + ": run bash -c '…' for those. "
	}
	return note + ". "
}

var (
	pipefailMu      sync.Mutex
	pipefailChecked bool
	pipefailOK      bool
)

// shellHasPipefail reports whether sh accepts -o pipefail (bash, recent dash and
// busybox ash). Without it, `make | tail; echo $?` reports tail's status, and a model
// reads a failed build as a passing one. The answer is kept once sh has been found.
func shellHasPipefail() bool {
	pipefailMu.Lock()
	defer pipefailMu.Unlock()
	if !pipefailChecked {
		if _, err := exec.LookPath("sh"); err != nil {
			return false
		}
		pipefailOK = exec.Command("sh", "-o", "pipefail", "-c", ":").Run() == nil
		pipefailChecked = true
	}
	return pipefailOK
}

// newRunCommand builds the shell command for cmdStr in workdir, bound to ctx, with
// pipefail set where sh has it and $HMJ_SCRATCH set to scratch, if any. When ctx ends the
// whole process group is killed, and WaitDelay bounds pipe drains.
func newRunCommand(ctx context.Context, cmdStr, workdir, scratch string, timeout time.Duration) *exec.Cmd {
	args := []string{"-c", cmdStr}
	if shellHasPipefail() {
		args = append([]string{"-o", "pipefail"}, args...)
	}
	cmd := exec.CommandContext(ctx, "sh", args...)
	cmd.Dir = workdir
	if scratch != "" {
		cmd.Env = append(os.Environ(), "HMJ_SCRATCH="+scratch)
	}
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

// exitOneOnNoMatch finds commands whose exit status 1 is an answer rather than a failure: grep
// and rg found no match, diff and cmp found a difference.
var exitOneOnNoMatch = regexp.MustCompile(`(^|[\s;&|(])(grep|egrep|fgrep|rg|diff|cmp)(\s|$)`)

// fdDup matches a redirection such as 2>&1, whose & does not end a command.
var fdDup = regexp.MustCompile(`[0-9]*[<>]&[0-9-]+`)

// feedsPipe reports whether the command that rest continues writes into a pipe: whether a |
// that is not || comes before the command ends at ;, &, && or ||, a newline or rest's end.
// Quoted text is skipped, so the | of grep -E 'a|b' is not taken for one.
func feedsPipe(rest string) bool {
	rest = fdDup.ReplaceAllString(rest, "")
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; c {
		case '\'', '"':
			end := strings.IndexByte(rest[i+1:], c)
			if end < 0 {
				return false
			}
			i += end + 1
		case '\\':
			i++
		case ';', '&', '\n':
			return false
		case '|':
			return i+1 == len(rest) || rest[i+1] != '|'
		}
	}
	return false
}

// runCommandExitNotice describes a failed run of cmdStr, such as "[Exit code 2]".
func runCommandExitNotice(cmdStr string, runErr error) string {
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return "[Exit code 0, but " + heldOutputNote + "]"
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		code := exitErr.ExitCode()
		// Under pipefail, `grep -l "<failure" *.xml | head` that finds no failing test ends
		// with grep's 1, which reads as the opposite of what it means.
		if code == 1 && runErr.Error() == "exit status 1" {
			if ms := exitOneOnNoMatch.FindAllStringSubmatchIndex(cmdStr, -1); ms != nil {
				m, piped := ms[0], false
				for _, c := range ms {
					if feedsPipe(cmdStr[c[1]:]) {
						m, piped = c, true
						break
					}
				}
				name := cmdStr[m[4]:m[5]]
				answer := "matches nothing"
				if name == "diff" || name == "cmp" {
					answer = "finds a difference"
				}
				// Without a pipe after it, its 1 is the command's own status, pipefail or not.
				how := ""
				if piped {
					how = ", and pipefail passes that on"
				}
				return fmt.Sprintf("[Exit code 1: the command runs %s, which also exits 1 when it %s%s]", name, answer, how)
			}
		}
		// Under pipefail, `seq 1 1000000 | head -1` ends with the status of seq, killed
		// by SIGPIPE when head stopped reading: not a failure of the command's work.
		if code == 128+int(syscall.SIGPIPE) {
			return fmt.Sprintf("[Exit code %d: a command in a pipeline was stopped by SIGPIPE because the next one, such as head, stopped reading; usually not an error]", code)
		}
		// "exit status N" is just the ExitError's own text; repeating it
		// after the code wastes tokens. Keep both only when they differ.
		if runErr.Error() == "exit status "+strconv.Itoa(code) {
			return fmt.Sprintf("[Exit code %d]", code)
		}
		return fmt.Sprintf("[Exit code %d: %v]", code, runErr)
	}
	return fmt.Sprintf("[%v]", runErr)
}

// heldOutputNote explains exec.ErrWaitDelay, which Wait reports only for a command that
// exited 0: a process it left running, such as a server it started or the sleep of a subshell
// it killed, kept the output pipe open, and hmj closed it a moment later.
const heldOutputNote = "a process it left running (such as a server it started, or the sleep of a subshell it killed) still held its output open, so hmj stopped reading there"

// inPlaceEdit finds sed or perl editing files in place (-i, -i.bak, -pi, --in-place).
var inPlaceEdit = regexp.MustCompile(`(^|[\s;&|(])(sed|perl)(\s[^;&|\n]*?)?\s(-[a-zA-Z]*i\S*|--in-place\S*)`)

// inPlaceEditNote is a note for a command that edits files with sed -i or perl -i: such a
// change shows the user no diff and leaves no backup, as a session's sed -i on a source file
// and strings.xml did. Commands that name the scratch directory get none.
func inPlaceEditNote(ctx context.Context, cmdStr string) string {
	m := inPlaceEdit.FindStringSubmatch(cmdStr)
	if m == nil || strings.Contains(cmdStr, "HMJ_SCRATCH") {
		return ""
	}
	if dir := scratchDirFrom(ctx); dir != "" && strings.Contains(cmdStr, dir) {
		return ""
	}
	return fmt.Sprintf("[note: %s %s changes files with no diff shown and no backup kept; for project files use edit_file]", m[2], m[4])
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
	if in.Background {
		return startBackgroundJob(ctx, cmdStr, workdir, in.TimeoutSeconds), nil
	}

	// One writer as both streams: exec then gives the command a single pipe for the two, so
	// stdout and stderr arrive in the order the command wrote them.
	out := newCommandOutput(cmdStr, progressFrom(ctx))
	started := time.Now()
	p, err := startShell(cmdStr, workdir, scratchDirFrom(ctx), out, t.timeoutFor(in))
	if err != nil {
		return withNotice(out.finish(), fmt.Sprintf("[%v]", err)), nil
	}

	select {
	case <-p.done:
	case <-ctx.Done():
		p.stop()
	case <-backgroundKeyFrom(ctx):
		return moveToBackground(ctx, p, cmdStr, out, started), nil
	}
	res := out.finish()

	switch {
	case p.stopped.Load():
		return withNotice(res, "[command cancelled by user]"), nil
	case p.timedOut.Load():
		return withNotice(res, "[command timed out]"), nil
	case p.err != nil:
		res = withNotice(res, runCommandExitNotice(cmdStr, p.err))
	case res == "":
		res = "[command executed successfully with no output]"
	}
	if note := inPlaceEditNote(ctx, cmdStr); note != "" {
		res = withNotice(res, note)
	}
	return res, nil
}

// startBackgroundJob starts cmdStr as a background job and returns the call's result.
func startBackgroundJob(ctx context.Context, cmdStr, workdir string, timeoutSeconds int) string {
	timeout := time.Duration(MaxBackgroundJobSeconds) * time.Second
	if timeoutSeconds > 0 {
		timeout = time.Duration(min(timeoutSeconds, MaxBackgroundJobSeconds)) * time.Second
	}
	j, err := Jobs.start(scratchDirFrom(ctx), cmdStr, workdir, timeout)
	if err != nil {
		return fmt.Sprintf("[%v]", err)
	}
	return fmt.Sprintf("[started background job %d; %s. You will be told when it ends, with its exit code and last lines; meanwhile end your turn rather than wait, and the user can talk to you. check_command reports on it or stops it.]", j.ID, j.out.where())
}

// moveToBackground hands a running foreground command to the job registry when the user
// presses Ctrl+B, and returns the call's result: the output so far and the job's number. The
// user pressed it to have the prompt back, so the turn ends with this call (see
// WithBackgroundKey).
func moveToBackground(ctx context.Context, p *shellProcess, cmdStr string, out *commandOutput, started time.Time) string {
	var soFar string
	j := Jobs.adopt(scratchDirFrom(ctx), cmdStr, p, started, time.Duration(MaxBackgroundJobSeconds)*time.Second, func() string {
		soFar = out.finish()
		return soFar
	})
	reportMovedToBackground(ctx, j.ID)
	return withNotice(soFar, fmt.Sprintf("[moved to the background by the user as job %d after %s, which ended your turn; the output so far is above, and %s. You will be told when it ends, with its exit code and last lines. check_command reports on it or stops it.]", j.ID, formatElapsed(time.Since(started)), j.out.where()))
}
