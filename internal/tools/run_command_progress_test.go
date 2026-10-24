package tools

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestLastLineTrackerFollowsTheLatestLine checks which line a progress report names as output
// arrives in pieces: the line being written once it has text, else the last full line.
func TestLastLineTrackerFollowsTheLatestLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"a full line", []string{"compiling\n"}, "compiling"},
		{"a line being written", []string{"done\n", "Tests:..."}, "Tests:..."},
		{"a line growing across writes", []string{"Tests:.", "..", "."}, "Tests:...."},
		{"blank lines keep the last text", []string{"BUILD SUCCESSFUL\n", "\n  \n"}, "BUILD SUCCESSFUL"},
		{"a carriage return redraws the line", []string{" 10%\r 55%\r", "100%"}, "100%"},
		{"a redraw shows until replaced", []string{" 10%\r 55%\r"}, "55%"},
		{"CRLF ends a line", []string{"first\r\n", "\r\n"}, "first"},
		{"CRLF split across writes", []string{"first\r", "\nsecond\r", "\n"}, "second"},
		{"invalid UTF-8 is dropped", []string{"caf\xc3"}, "caf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr lastLineTracker
			var got string
			for _, c := range tc.chunks {
				got = tr.Write([]byte(c))
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLastLineTrackerKeepsTheStartOfALongLine checks that a line longer than the kept length
// keeps its start, which is what the terminal shows.
func TestLastLineTrackerKeepsTheStartOfALongLine(t *testing.T) {
	var tr lastLineTracker
	got := tr.Write([]byte("start" + strings.Repeat("x", 3*maxProgressLineBytes) + "\n"))
	if len(got) != maxProgressLineBytes || !strings.HasPrefix(got, "start") {
		t.Errorf("got %d bytes starting %q, want %d starting \"start\"", len(got), got[:min(len(got), 10)], maxProgressLineBytes)
	}
}

// TestRunCommandReportsProgress checks that run_command reports each output line to the
// context's progress function, from stdout and stderr, and keeps doing so past its output cap.
func TestRunCommandReportsProgress(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	ctx := WithProgress(context.Background(), func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})
	cmd := `echo first; sleep 0.05; echo second >&2; sleep 0.05; head -c 70000 /dev/zero | tr '\0' a; echo; echo after-the-cap`
	out, err := (&RunCommandTool{}).Execute(ctx, mustJSON(t, map[string]any{"command": cmd}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "omitted") {
		t.Fatalf("the output did not pass the cap: %d bytes", len(out))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"first", "second"} {
		if !slices.Contains(lines, want) {
			t.Errorf("no report of %q in %q", want, lines)
		}
	}
	if len(lines) == 0 || lines[len(lines)-1] != "after-the-cap" {
		t.Errorf("reports %q do not end with the line after the cap", lines[max(len(lines)-3, 0):])
	}
}

// TestRunCommandWithoutProgressReportsNothing checks that a run with no progress function in
// its context works as before.
func TestRunCommandWithoutProgressReportsNothing(t *testing.T) {
	out, err := (&RunCommandTool{}).Execute(context.Background(), mustJSON(t, map[string]any{"command": "echo hi"}))
	if err != nil || out != "hi" {
		t.Errorf("Execute = %q, %v; want \"hi\"", out, err)
	}
}
