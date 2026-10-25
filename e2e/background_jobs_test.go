package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitCommand is a shell loop that waits, bounded, for the file release in the workspace,
// so a failed test leaves no loop running.
const waitCommand = "i=0; while [ ! -f release ] && [ $i -lt 600 ]; do i=$((i+1)); sleep 0.05; done"

// TestBackgroundJobWakesTheIdlePrompt checks the whole background flow: the model starts a
// job and ends its turn, the prompt is free and no request is made while the job runs, and
// when the job ends a turn starts by itself with its exit code and output.
func TestBackgroundJobWakesTheIdlePrompt(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	p.Queue(
		callTool("run_command", map[string]any{"command": waitCommand + "; echo suite-passed", "background": true}),
		say("The suite is running; I will be told when it ends."),
		say("Earlier answer while it runs."),
		say("The suite passed."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("run the suite\r")
	s.Expect("run_command: i=0")
	s.Expect("(background)")
	s.Expect("[started background job 1;")
	s.Expect("The suite is running")
	s.ExpectRegexp(`\[model: [^\]]+1 background job running\]\n> `)

	// The user can talk to the model meanwhile.
	s.Type("are you there?\r")
	s.Expect("Earlier answer while it runs.")
	s.ExpectPrompt()
	time.Sleep(300 * time.Millisecond)
	if n := len(p.Requests()); n != 3 {
		t.Fatalf("%d requests while the job runs, want 3", n)
	}

	e.WriteFile("release", "")
	s.Expect("[background job 1 finished: exit 0 after ")
	s.Expect("The suite passed.")
	s.ExpectPrompt()
	last := p.LastRequest()
	if !strings.Contains(last, "[background job 1 finished: exit 0") || !strings.Contains(last, "suite-passed") {
		t.Errorf("the woken turn's request lacks the notice and output: %s", tail(last, 600))
	}
	s.Quit()
}

// TestJobEndingMidTurnJoinsTheNextToolResult checks that a job ending while the model works is
// told with the result of the tool call that follows, without waiting for the turn to end.
func TestJobEndingMidTurnJoinsTheNextToolResult(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	p.Queue(
		callTool("run_command", map[string]any{"command": "echo quick-job-output", "background": true}),
		callTool("run_command", map[string]any{"command": "sleep 0.5"}),
		say("Both done."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("go\r")
	s.Expect("[background job 1 finished: exit 0")
	s.Expect("Both done.")
	s.ExpectPrompt()
	last := p.LastRequest()
	if !strings.Contains(last, "background job 1 finished: exit 0") || !strings.Contains(last, "quick-job-output") {
		t.Errorf("the next request lacks the job's notice: %s", tail(last, 600))
	}
	if n := len(p.Requests()); n != 3 {
		t.Errorf("%d requests, want 3: the notice should not start a turn of its own", n)
	}
	s.Quit()
}

// TestCtrlBMovesARunningCommandToTheBackground checks that Ctrl+B under a running command
// ends the call with a job number while the command runs on, and gives the prompt back at
// once: the turn ends, skipping the step's other calls, with no further request. The job's
// end then wakes the prompt.
func TestCtrlBMovesARunningCommandToTheBackground(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	p.Queue(
		callTools(
			toolCall{"run_command", map[string]any{"command": "echo $$ > child.pid; echo started-long; " + waitCommand + "; echo long-finished"}},
			toolCall{"run_command", map[string]any{"command": "touch second-ran"}},
		),
		say("It finished."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("run the long one\r")
	s.ExpectScreen("the status line offering Ctrl+B", func(v *vtScreen) bool {
		return v.RowOf("Ctrl+B: background") >= 0
	})
	pid := readPID(t, filepath.Join(e.Workspace, "child.pid"))
	s.Type("\x02")
	s.Expect("[moved to the background by the user as job 1 after ")
	s.Expect("[job 1 runs in the background; the model hears of it with your next message, or when it ends]")
	s.ExpectPrompt()
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the command was killed instead of moved")
	}
	if n := len(p.Requests()); n != 1 {
		t.Errorf("%d requests after Ctrl+B, want 1: the turn should end with the moved command", n)
	}
	if _, err := os.Stat(filepath.Join(e.Workspace, "second-ran")); err == nil {
		t.Error("the step's second call ran after Ctrl+B ended the turn")
	}

	e.WriteFile("release", "")
	s.Expect("[background job 1 finished: exit 0 after ")
	s.Expect("It finished.")
	s.ExpectPrompt()
	last := p.LastRequest()
	for _, want := range []string{"which ended your turn", "long-finished", "cancelled by user before this tool ran"} {
		if !strings.Contains(last, want) {
			t.Errorf("the woken turn's request lacks %q: %s", want, tail(last, 900))
		}
	}
	s.Quit()
}

// TestJobEndingWhileTypingKeepsTheDraft checks that a job ending while the user is typing
// starts no turn: the draft stays, and the notice goes with the message it becomes.
func TestJobEndingWhileTypingKeepsTheDraft(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	p.Queue(
		callTool("run_command", map[string]any{"command": waitCommand + "; echo build-ok", "background": true}),
		say("Started."),
		say("Got both."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("build it\r")
	s.Expect("Started.")
	s.ExpectPrompt()
	s.TypeSlowly("and then")
	e.WriteFile("release", "")
	time.Sleep(700 * time.Millisecond)
	if n := len(p.Requests()); n != 2 {
		t.Fatalf("%d requests, want 2: a turn started under the typed text", n)
	}
	s.Type(" deploy\r")
	s.Expect("[background job 1 finished: exit 0")
	s.Expect("Got both.")
	last := p.LastRequest()
	if !strings.Contains(last, "build-ok") || !strings.Contains(last, "and then deploy") {
		t.Errorf("the request lacks the notice or the message: %s", tail(last, 600))
	}
	s.ExpectPrompt()
	s.Quit()
}

// TestJobsCommandAndExitStopJobs checks that /jobs lists the jobs and stops the one picked
// without waking the prompt, that jobs are numbered per conversation and /jobs names the
// conversation of another's, and that leaving hmj stops the jobs still running.
func TestJobsCommandAndExitStopJobs(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	p.Queue(
		callTool("run_command", map[string]any{"command": "echo $$ > one.pid; exec sleep 60", "background": true}),
		callTool("run_command", map[string]any{"command": "echo $$ > two.pid; exec sleep 60", "background": true}),
		say("Two jobs started."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("start two\r")
	s.Expect("Two jobs started.")
	s.ExpectPrompt()
	one := readPID(t, filepath.Join(e.Workspace, "one.pid"))
	two := readPID(t, filepath.Join(e.Workspace, "two.pid"))

	s.Type("/jobs\r")
	s.ExpectRegexp(`job 1  running \d+s  echo \$\$ > one.pid; exec sleep 60\njob 2  running`)
	s.Expect("stop a job (Enter stops it, Esc keeps them running)")
	s.Type("\r") // the first running job
	s.Expect("[job 1 stopped; the model hears of it with your next message]")
	s.ExpectPrompt()
	waitUntil(t, "job 1 is gone", func() bool { return syscall.Kill(one, 0) != nil })
	time.Sleep(300 * time.Millisecond)
	if n := len(p.Requests()); n != 3 {
		t.Errorf("%d requests after /jobs stop, want 3: stopping a job should not start a turn", n)
	}

	// A new conversation numbers its jobs from 1; /jobs names the conversation of the others
	// and stops the one picked by its conversation, not by its number alone.
	p.Queue(
		callTool("run_command", map[string]any{"command": "echo $$ > three.pid; exec sleep 60", "background": true}),
		say("Third started."),
	)
	s.Type("/new\r")
	s.Expect("created conversation")
	s.ExpectPrompt()
	s.Type("start one more\r")
	s.Expect("[started background job 1;")
	s.Expect("Third started.")
	s.ExpectPrompt()
	three := readPID(t, filepath.Join(e.Workspace, "three.pid"))
	s.Type("/jobs\r")
	s.ExpectRegexp(`job 2  running \d+s  echo \$\$ > two.pid; exec sleep 60  \(conversation 1\)\njob 1  running \d+s  echo \$\$ > three.pid`)
	s.Expect("stop a job (Enter stops it, Esc keeps them running)")
	s.Type("\r") // job 2 of conversation 1
	s.Expect("[job 2 stopped; the model hears of it with your next message]")
	s.ExpectPrompt()
	waitUntil(t, "job 2 is gone", func() bool { return syscall.Kill(two, 0) != nil })
	if syscall.Kill(three, 0) != nil {
		t.Error("stopping job 2 of conversation 1 stopped this conversation's job 1")
	}

	s.Quit()
	waitUntil(t, "job 1 of the new conversation is stopped on exit", func() bool { return syscall.Kill(three, 0) != nil })
}

// readPID waits for a command to write its process id to path.
func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitUntil(t, "the command writes its pid", func() bool {
		data, err := os.ReadFile(path)
		if err != nil || !strings.HasSuffix(string(data), "\n") {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	})
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// TestIdlePromptWithAJobSleeps checks that hmj does not wake up while it waits at the prompt
// with a background job running: the job's end is noticed without polling for it.
func TestIdlePromptWithAJobSleeps(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	p.Queue(
		callTool("run_command", map[string]any{"command": waitCommand, "background": true}),
		say("Started."),
		say("It ended."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("start it\r")
	s.Expect("Started.")
	s.ExpectPrompt()
	time.Sleep(500 * time.Millisecond) // let startup work settle
	before := contextSwitches(t, s.cmd.Process.Pid)
	time.Sleep(2 * time.Second)
	if n := contextSwitches(t, s.cmd.Process.Pid) - before; n > 5 {
		t.Errorf("hmj woke %d times in 2s at an idle prompt", n)
	}

	e.WriteFile("release", "")
	s.Expect("It ended.")
	s.ExpectPrompt()
	s.Quit()
}

// contextSwitches sums the voluntary context switches of every thread of process pid.
func contextSwitches(t *testing.T, pid int) int {
	t.Helper()
	statuses, err := filepath.Glob("/proc/" + strconv.Itoa(pid) + "/task/*/status")
	if err != nil || len(statuses) == 0 {
		t.Fatalf("listing the threads of %d: %v", pid, err)
	}
	total := 0
	for _, path := range statuses {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // the thread ended
		}
		for line := range strings.Lines(string(data)) {
			if v, ok := strings.CutPrefix(line, "voluntary_ctxt_switches:"); ok {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					t.Fatal(err)
				}
				total += n
			}
		}
	}
	return total
}

// TestJobLostToARestartIsToldWithTheNextMessage checks that a job still running when hmj
// exits, whose end the model therefore never hears of, is shown to the user when the
// conversation is reopened and told to the model with the user's next message, with its last
// output, and only once.
func TestJobLostToARestartIsToldWithTheNextMessage(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	p.Queue(
		callTool("run_command", map[string]any{"command": "echo first-class-ok; echo $$ > job.pid; exec sleep 60", "background": true}),
		say("The tests are running."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("run the tests\r")
	s.Expect("The tests are running.")
	s.ExpectPrompt()
	pid := readPID(t, filepath.Join(e.Workspace, "job.pid"))
	s.Quit()
	waitUntil(t, "the job is stopped on exit", func() bool { return syscall.Kill(pid, 0) != nil })

	p.Queue(say("Job 1 was cut short; I will rerun it."), say("Nothing else."))
	s = e.Start()
	s.Expect("resumed conversation")
	s.Expect("[background job 1 never reported its end, as hmj stopped first; the model hears of it with your next message]")
	s.ExpectPrompt()
	s.Type("how did the tests go?\r")
	s.Expect("[background job 1 never reported its end, as hmj stopped first;")
	s.Expect("Job 1 was cut short")
	s.ExpectPrompt()
	last := p.LastRequest()
	if !strings.Contains(last, "first-class-ok") || !strings.Contains(last, "how did the tests go?") {
		t.Errorf("the request lacks the job's last output or the prompt: %s", tail(last, 800))
	}
	s.Type("anything else?\r")
	s.Expect("Nothing else.")
	s.ExpectPrompt()
	if n := strings.Count(p.LastRequest(), "never reported its end"); n != 1 {
		t.Errorf("the lost job was told %d times, want once", n)
	}
	s.Quit()

	// The resumed transcript shows the notice as a notice, not as the user's prompt.
	s = e.Start()
	s.Expect("resumed conversation")
	s.Expect("> you: how did the tests go?")
	s.ExpectPrompt()
	if screen := s.Screen(); strings.Contains(screen, "you: [background job 1") || strings.Contains(screen, "hears of it with your next message") {
		t.Errorf("the notice is shown as the prompt, or the told job as still lost:\n%s", tail(screen, 1500))
	}
	s.Quit()
}
