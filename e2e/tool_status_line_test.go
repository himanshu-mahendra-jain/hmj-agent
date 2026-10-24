package e2e

import (
	"strings"
	"testing"
)

// TestRunningCommandShowsItsLatestOutput checks the status line under a running command: it
// names the tool, counts the time, shows the turn's tokens so far and ends with the command's
// latest output, including a line still being written. Once the command ends the status line
// is gone and the result is printed.
func TestRunningCommandShowsItsLatestOutput(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	p.Queue(
		callTool("run_command", map[string]any{
			// bounded, so a failed test leaves no loop running
			"command": "echo compiling-module-a; printf 'Tests:..'; i=0; while [ ! -f release ] && [ $i -lt 400 ]; do i=$((i+1)); sleep 0.05; done; echo; echo finished-ok",
		}),
		say("All done."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("run it\r")
	s.ExpectScreen("the status line showing the line being written", func(v *vtScreen) bool {
		row := v.RowOf("Tests:..")
		if row < 0 {
			return false
		}
		line := strings.TrimRight(v.Lines()[row], " ")
		return strings.Contains(line, "run_command (") && strings.Contains(line, " out · ") && strings.HasSuffix(line, " · Tests:..")
	})
	e.WriteFile("release", "")
	s.Expect("finished-ok")
	s.Expect("All done.")
	s.ExpectPrompt()
	if v := s.ScreenState(); v.RowOf("run_command (") >= 0 {
		t.Errorf("the status line was left on screen:\n%s", v.Dump())
	}
	s.Quit()
}
