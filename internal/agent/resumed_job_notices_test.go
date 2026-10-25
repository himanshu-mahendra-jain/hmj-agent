package agent

import (
	"slices"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/terminal/theme"
)

// jobNotice is a finish notice as the job registry writes it, with blank lines in its output.
func jobNotice(id, output string) string {
	return "[background job " + id + " finished: exit 0 after 45s: ./gradlew test]\n" + output + "\n[its output is in /scratch/jobs/job" + id + ".log]"
}

// TestJobNoticesSplitFromThePrompt checks that the notices withJobNotices puts before a prompt
// come apart from it again, also when a job's output has blank lines and with no prompt at all.
func TestJobNoticesSplitFromThePrompt(t *testing.T) {
	notices := []string{jobNotice("1", "\nOK (11 tests)\n"), jobNotice("2", "BUILD SUCCESSFUL")}
	for _, input := range []string{"", "now run the rest", "[background job 9 finished: typed by the user]"} {
		gotNotices, gotInput := session.SplitJobNotices(withJobNotices(notices, input))
		if !slices.Equal(gotNotices, notices) || gotInput != input {
			t.Errorf("input %q: got %q and %q", input, gotNotices, gotInput)
		}
	}
	if n, in := session.SplitJobNotices("fix the build"); n != nil || in != "fix the build" {
		t.Errorf("a plain prompt: got %q and %q", n, in)
	}
}

// TestResumedJobNoticesAreNotThePrompt checks that a resumed transcript shows a turn started by
// finished jobs as a turn shows it, each notice's first line, not as a prompt holding the jobs'
// output, and that only prompts the user typed are counted.
func TestResumedJobNoticesAreNotThePrompt(t *testing.T) {
	theme.SetEnabled(false)
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "run the tests"},
		{Role: "assistant", Content: "Started."},
		{Role: "user", Content: withJobNotices([]string{jobNotice("5", "\nOK (11 tests)")}, "")},
		{Role: "assistant", Content: "All pass."},
		{Role: "user", Content: withJobNotices([]string{jobNotice("6", "done")}, "and lint?")},
	}}
	out := captureOutput(t, func() { PrintConversationHistory(conv) })
	for _, want := range []string{"Started.\n\n[background job 5 finished: exit 0 after 45s: ./gradlew test]\n", "> you: and lint?\n\n[background job 6 finished"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "you: [background") || strings.Contains(out, "OK (11 tests)") || strings.Contains(out, "its output is in") {
		t.Errorf("a job notice is shown as the user's prompt, or whole:\n%s", out)
	}
	if got := conv.PromptCount(); got != 2 {
		t.Errorf("PromptCount = %d, want 2", got)
	}
}

// TestResumedToolResultKeepsItsJobNoticesApart checks that the notices of jobs that finished
// while a tool ran, which end its result, show after the result by their first line, as in the
// turn, and do not take the place of the command's own last lines.
func TestResumedToolResultKeepsItsJobNoticesApart(t *testing.T) {
	theme.SetEnabled(false)
	var lines []string
	for i := range 12 {
		lines = append(lines, "built module "+string(rune('a'+i)))
	}
	output := strings.Join(lines, "\n")
	notices := []string{jobNotice("3", "\nOK (11 tests)\n"), jobNotice("4", "done")}
	content := output + "\n\n" + strings.Join(notices, "\n\n")
	if gotOutput, gotNotices := session.SplitTrailingJobNotices(content); gotOutput != output || !slices.Equal(gotNotices, notices) {
		t.Fatalf("got %q and %q", gotOutput, gotNotices)
	}
	if gotOutput, gotNotices := session.SplitTrailingJobNotices(output); gotOutput != output || gotNotices != nil {
		t.Errorf("a result without notices: got %q and %q", gotOutput, gotNotices)
	}
	quoted := "log:\n\n[background job 1 finished: exit 0 after 1s: x]\nmore\n[its output is in /x.log]\nand after"
	if gotOutput, gotNotices := session.SplitTrailingJobNotices(quoted); gotOutput != quoted || gotNotices != nil {
		t.Errorf("a notice quoted inside the output is split off: %q and %q", gotOutput, gotNotices)
	}

	call := chat.ToolCall{ID: "c1", Type: "function", Function: chat.ToolCallFunction{Name: "run_command", Arguments: `{"command":"./gradlew build"}`}}
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "build it"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{call}},
		{Role: "tool", ToolCallID: "c1", Content: content},
	}}
	out := captureOutput(t, func() { PrintConversationHistory(conv) })
	for _, want := range []string{"built module l\n[background job 3 finished: exit 0 after 45s: ./gradlew test]\n[background job 4 finished"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "OK (11 tests)") || strings.Contains(out, "its output is in") {
		t.Errorf("a job notice is shown whole inside the result:\n%s", out)
	}
}
