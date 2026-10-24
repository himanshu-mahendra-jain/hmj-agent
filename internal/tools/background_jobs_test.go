package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// jobNumber reads the job number out of a run_command result.
func jobNumber(t *testing.T, result string) int {
	t.Helper()
	m := regexp.MustCompile(`job (\d+)`).FindStringSubmatch(result)
	if m == nil {
		t.Fatalf("no job number in %q", result)
	}
	id, _ := strconv.Atoi(m[1])
	return id
}

// pidIn waits for a command to write its process id to path and returns it.
func pidIn(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(data), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBackgroundJobReturnsAtOnceAndReportsItsEnd checks a background run_command: the call
// returns at once with a job number, check_command reports it running, and once it ends its
// exit code and last lines are told once, to its own conversation only.
func TestBackgroundJobReturnsAtOnceAndReportsItsEnd(t *testing.T) {
	scratch := t.TempDir()
	ctx := WithScratchDir(context.Background(), scratch)
	release := filepath.Join(t.TempDir(), "release")

	start := time.Now()
	out, err := (&RunCommandTool{}).Execute(ctx, mustJSON(t, map[string]any{
		"command":    "echo first; while [ ! -f " + release + " ]; do sleep 0.02; done; echo last; exit 3",
		"background": true,
	}))
	if took := time.Since(start); err != nil || took > 200*time.Millisecond {
		t.Fatalf("background start: %q, %v after %v; want a result within 200ms", out, err, took)
	}
	if !strings.HasPrefix(out, "[started background job ") || !strings.Contains(out, filepath.Join(scratch, "jobs")) {
		t.Errorf("start result = %q", out)
	}
	id := jobNumber(t, out)
	if n := Jobs.Running(scratch); n != 1 {
		t.Errorf("Running = %d, want 1", n)
	}

	check := func(args map[string]any) string {
		t.Helper()
		args["job"] = id
		res, err := (&CheckCommandTool{}).Execute(ctx, mustJSON(t, args))
		if err != nil {
			t.Fatalf("check_command: %v", err)
		}
		return res
	}
	if res := check(map[string]any{}); !strings.Contains(res, "is running, for ") {
		t.Errorf("running job reported as %q", res)
	}
	if got := Jobs.TakeFinished(scratch); len(got) != 0 {
		t.Errorf("finish notices while running: %q", got)
	}

	stopWake := make(chan struct{})
	defer close(stopWake)
	otherWake := Jobs.Wake(t.TempDir(), stopWake)
	wake := Jobs.Wake(scratch, stopWake)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	case <-time.After(5 * time.Second):
		t.Fatal("no wake when the job ended")
	}
	select {
	case <-otherWake:
		t.Error("another conversation was woken")
	default:
	}

	notices := Jobs.TakeFinished(scratch)
	if len(notices) != 1 {
		t.Fatalf("finish notices = %q, want one", notices)
	}
	n := notices[0]
	if !strings.HasPrefix(n, "[background job "+strconv.Itoa(id)+" finished: exit 3 after ") || !strings.Contains(n, "first\nlast\n[its output is in ") {
		t.Errorf("finish notice = %q", n)
	}
	if again := Jobs.TakeFinished(scratch); len(again) != 0 {
		t.Errorf("told twice: %q", again)
	}
	if res := check(map[string]any{}); !strings.Contains(res, "finished: exit 3") {
		t.Errorf("finished job reported as %q", res)
	}
}

// TestBackgroundJobOutlivesTheTurnAndStops checks that a background job keeps running when
// the turn that started it is halted, that check_command's stop kills its whole process
// group, and that StopAll stops the rest, as hmj does on exit.
func TestBackgroundJobOutlivesTheTurnAndStops(t *testing.T) {
	dir := t.TempDir()
	turn, halt := context.WithCancel(WithScratchDir(context.Background(), dir))
	start := func(name string) (int, int) {
		out, err := (&RunCommandTool{}).Execute(turn, mustJSON(t, map[string]any{
			"command":    "sleep 60 & echo $! > " + filepath.Join(dir, name) + "; wait",
			"background": true,
		}))
		if err != nil {
			t.Fatal(err)
		}
		return jobNumber(t, out), pidIn(t, filepath.Join(dir, name))
	}
	id, child := start("a.pid")
	_, other := start("b.pid")

	halt()
	time.Sleep(100 * time.Millisecond)
	if !alive(child) || !alive(other) {
		t.Fatal("halting the turn killed a background job")
	}

	res, err := (&CheckCommandTool{}).Execute(context.Background(), mustJSON(t, map[string]any{"job": id, "stop": true}))
	if err != nil || !strings.Contains(res, "finished: stopped after ") {
		t.Errorf("stop: %q, %v", res, err)
	}
	waitGone(t, child)
	if !alive(other) {
		t.Error("stopping one job killed another")
	}

	Jobs.StopAll()
	waitGone(t, other)

	if _, err := (&CheckCommandTool{}).Execute(context.Background(), `{"job": 999999}`); err == nil || !strings.Contains(err.Error(), "no background job 999999") {
		t.Errorf("unknown job: %v", err)
	}
}

// TestCtrlBMovesARunningCommandToTheBackground checks the user's Ctrl+B: the foreground call
// returns with the output so far and a job number, the command runs on, and its log holds all
// its output in order.
func TestCtrlBMovesARunningCommandToTheBackground(t *testing.T) {
	scratch := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	keys := make(chan struct{}, 1)
	ctx := WithBackgroundKey(WithScratchDir(context.Background(), scratch), keys)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := (&RunCommandTool{}).Execute(ctx, mustJSON(t, map[string]any{
			"command": "echo before; while [ ! -f " + release + " ]; do sleep 0.02; done; echo after",
		}))
		done <- result{out, err}
	}()
	time.Sleep(200 * time.Millisecond) // "before" is printed
	keys <- struct{}{}

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+B did not end the foreground call")
	}
	if r.err != nil || !strings.HasPrefix(r.out, "before\n[moved to the background by the user as job ") {
		t.Fatalf("result = %q, %v", r.out, r.err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	j := Jobs.get(jobNumber(t, r.out))
	<-j.proc.done
	Jobs.waitEnded(j)
	logData, _ := os.ReadFile(j.out.path)
	if string(logData) != "before\nafter\n" {
		t.Errorf("log = %q, want before and after in order", logData)
	}
	if notices := Jobs.TakeFinished(scratch); len(notices) != 1 || !strings.Contains(notices[0], "finished: exit 0") {
		t.Errorf("finish notices = %q", notices)
	}
}

// TestCheckCommandWaitIsCapped checks that wait_seconds over the cap still returns, with the
// job running, once the cap has passed or the turn is halted.
func TestCheckCommandWaitIsCapped(t *testing.T) {
	ctx, halt := context.WithCancel(WithScratchDir(context.Background(), t.TempDir()))
	out, _ := (&RunCommandTool{}).Execute(ctx, mustJSON(t, map[string]any{"command": "sleep 30", "background": true}))
	id := jobNumber(t, out)
	defer func() { _ = Jobs.Stop(id) }()
	time.AfterFunc(100*time.Millisecond, halt)
	start := time.Now()
	res, err := (&CheckCommandTool{}).Execute(ctx, mustJSON(t, map[string]any{"job": id, "wait_seconds": 100000}))
	if err != nil || !strings.Contains(res, "is running") || time.Since(start) > 2*time.Second {
		t.Errorf("halted wait: %q, %v after %v", res, err, time.Since(start))
	}
}

// TestCtrlBLiftsTheForegroundTimeout checks that a command moved to the background runs past
// the timeout it had in the foreground and ends with its own exit code.
func TestCtrlBLiftsTheForegroundTimeout(t *testing.T) {
	scratch := t.TempDir()
	keys := make(chan struct{}, 1)
	ctx := WithBackgroundKey(WithScratchDir(context.Background(), scratch), keys)
	done := make(chan string, 1)
	go func() {
		out, _ := (&RunCommandTool{}).Execute(ctx, mustJSON(t, map[string]any{"command": "sleep 1.6; echo outlived", "timeout_seconds": 1}))
		done <- out
	}()
	time.Sleep(300 * time.Millisecond)
	keys <- struct{}{}
	j := Jobs.get(jobNumber(t, <-done))
	<-j.proc.done
	Jobs.waitEnded(j)
	if res := Jobs.report(j, 5, 1024); !strings.Contains(res, "finished: exit 0 after 2s") || !strings.Contains(res, "outlived") {
		t.Errorf("report = %q, want exit 0 after the foreground timeout", res)
	}
}
