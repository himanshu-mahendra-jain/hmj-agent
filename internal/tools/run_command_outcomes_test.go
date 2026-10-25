package tools

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// run_command turns each way a command can end into a short notice for the model.
func TestRunCommandOutcomes(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{"success without output", `{"command":"true"}`, "[command executed successfully with no output]"},
		{"exit code alone", `{"command":"echo out; exit 3"}`, "out\n[Exit code 3]"},
		{"pipeline fails with its first command", `{"command":"false | cat; echo status=$?"}`, "status=1"},
		{"pipeline reader stopped early", `{"command":"yes | head -1"}`, "y\n[Exit code 141: a command in a pipeline was stopped by SIGPIPE because the next one, such as head, stopped reading; usually not an error]"},
		{"grep matching nothing", `{"command":"echo a | grep -l b | head"}`, "[Exit code 1: the command runs grep, which also exits 1 when it matches nothing, and pipefail passes that on]"},
		{"grep piped after a redirection", `{"command":"echo a | grep b 2>&1 | head"}`, "[Exit code 1: the command runs grep, which also exits 1 when it matches nothing, and pipefail passes that on]"},
		{"grep ending the command", `{"command":"echo a >/dev/null; grep -E 'BUILD|FAILED' /dev/null"}`, "[Exit code 1: the command runs grep, which also exits 1 when it matches nothing]"},
		{"grep before ||", `{"command":"echo a | grep b || exit 1"}`, "[Exit code 1: the command runs grep, which also exits 1 when it matches nothing]"},
		{"cmp finding a difference", `{"command":"echo a | cmp - /dev/null >/dev/null 2>&1"}`, "[Exit code 1: the command runs cmp, which also exits 1 when it finds a difference]"},
		{"exit 1 without such a command", `{"command":"echo groups; exit 1"}`, "groups\n[Exit code 1]"},
		{"killed by a signal", `{"command":"kill -9 $$"}`, "[Exit code -1: signal: killed]"},
		{"per-call timeout", `{"command":"sleep 30","timeout_seconds":1}`, "[command timed out]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&RunCommandTool{}).Execute(context.Background(), tc.args)
			if err != nil || out != tc.want {
				t.Errorf("got %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

// When the shell itself cannot be started, the start error is reported as the result.
func TestRunCommandShellMissing(t *testing.T) {
	t.Setenv("PATH", "")
	out, err := (&RunCommandTool{}).Execute(context.Background(), `{"command":"echo hi"}`)
	if err != nil || !strings.HasPrefix(out, "[") || !strings.Contains(out, `"sh"`) {
		t.Errorf("got %q, %v; want a notice naming the missing shell", out, err)
	}
}

// The timeout comes from the per-call timeout_seconds (capped at 30 minutes), else the tool's
// configured duration or seconds, else 60s.
func TestRunCommandTimeoutSelection(t *testing.T) {
	tests := []struct {
		name string
		tool RunCommandTool
		in   runCommandInput
		want time.Duration
	}{
		{"default", RunCommandTool{}, runCommandInput{}, 60 * time.Second},
		{"configured seconds", RunCommandTool{TimeoutSeconds: 7}, runCommandInput{}, 7 * time.Second},
		{"configured duration wins over seconds", RunCommandTool{TimeoutDuration: time.Second, TimeoutSeconds: 7}, runCommandInput{}, time.Second},
		{"per call overrides", RunCommandTool{TimeoutSeconds: 7}, runCommandInput{TimeoutSeconds: 20}, 20 * time.Second},
		{"per call up to the cap", RunCommandTool{}, runCommandInput{TimeoutSeconds: 1000}, 1000 * time.Second},
		{"per call is capped", RunCommandTool{}, runCommandInput{TimeoutSeconds: 5000}, 30 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tool.timeoutFor(tc.in); got != tc.want {
				t.Errorf("timeoutFor = %v, want %v", got, tc.want)
			}
		})
	}
}

// Go files with many syntax errors get the first five, one per line, and a count of the
// rest (scanner.ErrorList's own message names only the first); a Python
// check that cannot run (cancelled) or passes adds no warning.
func TestFastSyntaxCheckLimitsAndSkips(t *testing.T) {
	src := "package p\n" + strings.Repeat("func (\n", 10)
	warn := FastSyntaxCheck(context.Background(), "bad.go", []byte(src))
	if !strings.HasPrefix(warn, "[syntax warning: ") || !strings.Contains(warn, "more error(s)") {
		t.Errorf("warning = %q, want the first errors and a count of the rest", warn)
	}
	if n := strings.Count(warn, "bad.go:"); n != 5 || strings.Count(warn, "\n") != 5 {
		t.Errorf("warning lists %d errors on %d lines, want 5 errors and the summary: %q", n, strings.Count(warn, "\n")+1, warn)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if warn := FastSyntaxCheck(ctx, "x.py", []byte("def (:\n")); warn != "" {
		t.Errorf("cancelled check warned: %q", warn)
	}
	if warn := FastSyntaxCheck(context.Background(), "ok.py", []byte("x = 1\n")); warn != "" {
		t.Errorf("valid Python warned: %q", warn)
	}
}

// The tool description warns that a filter leaving no lines fails a pipeline under pipefail.
func TestRunCommandDescriptionWarnsOfEmptyFilters(t *testing.T) {
	desc := (&RunCommandTool{}).Definition().Function.Description
	for _, want := range []string{"pipefail", "exit 1 when they match nothing", "redirect its output to a file"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description does not mention %q", want)
		}
	}
}

// A model writes bash; where sh is another shell, the description names it and what it lacks.
func TestRunCommandDescriptionNamesANonBashShell(t *testing.T) {
	cases := []struct {
		name    string
		hasBash bool
		want    string
	}{
		{"dash", true, "Here sh is dash, which lacks bash-only syntax such as $'\\t', [[ ]], arrays and <(…): run bash -c '…' for those. "},
		{"busybox", false, "Here sh is busybox, which lacks bash-only syntax such as $'\\t', [[ ]], arrays and <(…). "},
		{"bash", true, ""},
		{"", true, ""},
	}
	for _, tc := range cases {
		if got := shellNote(tc.name, tc.hasBash); got != tc.want {
			t.Errorf("shellNote(%q, %v) = %q, want %q", tc.name, tc.hasBash, got, tc.want)
		}
	}
	if desc := (&RunCommandTool{}).Definition().Function.Description; !strings.Contains(desc, hostShellNote()) {
		t.Errorf("description leaves out the host's shell note %q", hostShellNote())
	}
}

// TestCommandWhoseLeftoverHoldsItsOutputExitedZero checks that a command that exits 0 while a
// process it left running still holds its output, such as the sleep of a killed sampler
// subshell, is told as exit 0 with the reason, in the foreground and as a background job,
// rather than as Go's "exec: WaitDelay expired before I/O complete".
func TestCommandWhoseLeftoverHoldsItsOutputExitedZero(t *testing.T) {
	dir := t.TempDir()
	pidFile := dir + "/sleep.pid"
	command := "( sleep 5 & echo $! > " + pidFile + "; wait ) & echo started"
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	got := runCommand(t, command)
	if want := "started\n[Exit code 0, but a process it left running"; !strings.HasPrefix(got, want) || strings.Contains(got, "WaitDelay") {
		t.Errorf("foreground: got %q, want it to start %q", got, want)
	}

	r := NewJobRegistry()
	j, err := r.start(dir, command, dir, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.waitEnded(j)
	if got := r.report(j, 5, 1024); !strings.HasPrefix(got, "[background job 1 finished: exit 0 (a process it left running") {
		t.Errorf("background: got %q", got)
	}
}
