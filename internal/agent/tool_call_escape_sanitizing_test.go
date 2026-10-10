package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hmj-agent/internal/terminal/theme"
)

// sgrSeq matches one colour (SGR) escape, the only kind the agent itself prints.
var sgrSeq = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// assertNoInjectedControls fails when out, with the agent's own colour escapes removed,
// still holds an escape, BEL or carriage return: something model-supplied reached the
// terminal raw and could move the cursor, rewrite the line or set the clipboard.
func assertNoInjectedControls(t *testing.T, out string) {
	t.Helper()
	rest := sgrSeq.ReplaceAllString(out, "")
	if i := strings.IndexAny(rest, "\x1b\x07\r"); i >= 0 {
		t.Errorf("output holds a raw control byte %q at %d:\n%q", rest[i], i, out)
	}
}

// useColours turns colour output on for the test, so sanitizing is checked on the
// styled path rather than only the plain one.
func useColours(t *testing.T) {
	t.Helper()
	theme.SetEnabled(true)
	t.Cleanup(func() { theme.SetEnabled(false) })
}

// TestToolCallArgumentsCannotDriveTheTerminal checks that every tool argument shown
// before a tool runs is stripped of escape sequences, so a prompt-injected command
// cannot redraw itself as a harmless one.
func TestToolCallArgumentsCannotDriveTheTerminal(t *testing.T) {
	useColours(t)
	cases := []struct {
		name, args, want string
	}{
		{"run_command", `{"command":"rm -rf x\u001b[2K\rls -la"}`, "rm -rf x"},
		{"run_command", `{"command":"echo hi\u001b]52;c;cm0gLXJmIH4=\u0007"}`, "echo hi"},
		{"web_search", `{"query":"go\u001b[1A\u001b[2Kdocs"}`, "godocs"},
		{"fetch_url", `{"url":"http://a.test/\u001b]8;;http://b.test\u001b\\link\u001b]8;;\u001b\\"}`, "http://a.test/link"},
		{"read_file", `{"path":"a.txt\u001b[2K\rb.txt","offset":3}`, "a.txtb.txt"},
		{"read_file", `{"path":"a.txt","offset":"1\u001b[2K\rrm -rf ~"}`, "(offset: 1rm -rf ~)"},
		{"list_files", `{"path":"dir\u0007"}`, "dir"},
		{"write_file", `{"path":"f\u001b[31m.go","content":"x"}`, "f.go"},
		{"bad\u001b[2Kname", `{"x":"y"}`, "badname"},
		{"run_command", "not json \x1b]52;c;eA==\x07", "not json"},
	}
	for _, tc := range cases {
		out := captureOutput(t, func() { printExpandedToolCall(tc.name, tc.args) })
		if !strings.Contains(sgrSeq.ReplaceAllString(out, ""), tc.want) {
			t.Errorf("%s %s: output %q lacks %q", tc.name, tc.args, out, tc.want)
		}
		assertNoInjectedControls(t, out)
	}
}

// TestEditDiffCannotDriveTheTerminal checks that the diff shown for edit_file, both for a
// new file and for an edit of an existing one, carries no escape sequences from the old
// or new text, including in its unchanged context lines and its path.
func TestEditDiffCannotDriveTheTerminal(t *testing.T) {
	useColours(t)
	dir := t.TempDir()
	osc52 := "\x1b]52;c;cm0gLXJmIH4=\x07"

	created := filepath.Join(dir, "new\x1b[2K.txt")
	out := captureOutput(t, func() { printEditFileCall(created, "", "safe line"+osc52+"\nnext\r\n") })
	if !strings.Contains(out, "safe line") {
		t.Errorf("new-file diff lacks its text:\n%q", out)
	}
	assertNoInjectedControls(t, out)

	existing := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(existing, []byte("context\x07one\nold value\ncontext\x1b[1Atwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = captureOutput(t, func() { printEditFileCall(existing, "old value", "new"+osc52+" value") })
	if !strings.Contains(out, "contextone") || !strings.Contains(out, "value") {
		t.Errorf("edit diff lacks its context or changed text:\n%q", out)
	}
	assertNoInjectedControls(t, out)
}
