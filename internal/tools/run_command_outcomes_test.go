package tools

import (
	"context"
	"strings"
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
		{"cmp finding a difference", `{"command":"echo a | cmp - /dev/null >/dev/null 2>&1"}`, "[Exit code 1: the command runs cmp, which also exits 1 when it finds a difference, and pipefail passes that on]"},
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
