package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// cursorControl matches what accessibility mode must never write: cursor movement, erasing,
// scrolling, cursor hiding, or a carriage return that is not the start of a line ending. The
// exit's terminal restore, which shows the cursor, is allowed.
var cursorControl = regexp.MustCompile(`\x1b\[[0-9;]*[ABCDEFGHJKSTf]|\x1b\[\?25l|\x1b[78]|\r[^\n]`)

// expectAppendOnly fails the test if raw has a cursor control sequence.
func expectAppendOnly(t *testing.T, raw string) {
	t.Helper()
	if loc := cursorControl.FindStringIndex(raw); loc != nil {
		t.Errorf("accessibility mode wrote %q, in %q", raw[loc[0]:loc[1]], raw[max(loc[0]-80, 0):min(loc[1]+80, len(raw))])
	}
}

// TestAccessibleScreensAreAppendOnly walks every interactive screen in accessibility mode:
// /new and /resume with a delete confirmation, a tool-using turn with its status line, a
// background job, a compaction, /jobs, /history and its view of a turn, and the job's end. Each is a numbered list or a line prompt; nothing is redrawn.
func TestAccessibleScreensAreAppendOnly(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	s := e.Start("--accessible")
	s.ExpectPrompt()

	// Renamed, so /new keeps it rather than removing it unused; deleting it then leaves the
	// picker open.
	s.Type("/resume\r")
	s.Expect("r <n> <new name> renames")
	s.Type("r 1 keep\r")
	s.Expect("keep")
	s.Type("q\r")
	s.ExpectPrompt()
	s.Type("/new\r")
	s.Expect("created conversation")
	s.ExpectPrompt()
	s.Type("/resume\r")
	s.Expect("d <n> deletes")
	s.Type("d 1\r")
	s.Expect("type y to confirm:")
	s.Type("y\r")
	s.Expect("deleted [1] keep")
	s.Type("q\r")
	s.ExpectPrompt()

	p.Queue(
		callTool("run_command", map[string]any{"command": "echo tool-output"}),
		callTool("run_command", map[string]any{"command": waitCommand + "; touch ended", "background": true}),
		sayWithPrompt("Started.", overLimit),
	)
	p.QueueCompact(compactedWindow("WINDOW"))
	s.Type("run things\r")
	s.Expect("tool-output")
	s.Expect("Started.")

	s.ExpectPrompt()

	s.Type("/jobs\r")
	s.Expect("[compacted the conversation")  // on the first input after the turn
	s.ExpectRegexp(`stop a job\nCommands: `) // no Enter or Esc hints, which do not work here
	s.Expect("1. ")
	s.Type("q\r")
	s.ExpectPrompt()

	s.Type("/history\r")
	s.Expect("1. ")
	s.Type("1\r")
	s.Expect("View full turn in terminal")
	s.Type("4\r")
	s.Expect("run things")
	s.ExpectPrompt()

	// The prompt reads whole lines here and does not wake: the job's end comes with the next
	// message.
	e.WriteFile("release", "")
	waitUntil(t, "the job ends", func() bool {
		_, err := os.Stat(filepath.Join(e.Workspace, "ended"))
		return err == nil
	})
	time.Sleep(300 * time.Millisecond) // for hmj to reap it
	p.Queue(say("It ended."))
	s.Type("is it done?\r")
	s.Expect("[background job 1 finished: exit 0")
	s.Expect("It ended.")
	s.ExpectPrompt()
	s.Quit()
	expectAppendOnly(t, s.Raw())
}

// TestAccessibleFirstRunIsAppendOnly checks the first start without a config in accessibility
// mode: the starter config is written and the missing API key explained in plain lines.
func TestAccessibleFirstRunIsAppendOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.Config = filepath.Join(e.Home, ".hmj-first-run", "config.json")
	s := e.Start("--accessible")
	s.Expect("created a starter config at " + e.Config)
	s.Expect("API key")
	if code := s.Wait(); code != 1 {
		t.Errorf("exit status %d, want 1", code)
	}
	expectAppendOnly(t, s.Raw())
}
