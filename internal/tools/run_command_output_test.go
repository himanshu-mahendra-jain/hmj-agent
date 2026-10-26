package tools

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// runCommand runs command through run_command and returns its result.
func runCommand(t *testing.T, command string) string {
	t.Helper()
	out, err := (&RunCommandTool{}).Execute(context.Background(), mustJSON(t, map[string]any{"command": command}))
	if err != nil {
		t.Fatalf("Execute(%q): %v", command, err)
	}
	return out
}

// useCommandLogDir points saved outputs at a fresh directory for the test.
func useCommandLogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetCommandLogDir(dir)
	t.Cleanup(func() { SetCommandLogDir("") })
	return dir
}

var savedLogPath = regexp.MustCompile(`saved to (\S+\.log):`)

// TestRunCommandKeepsStdoutAndStderrInOrder checks that the two streams come back as one, in
// the order the command wrote them.
func TestRunCommandKeepsStdoutAndStderrInOrder(t *testing.T) {
	got := runCommand(t, "for i in 1 2 3; do echo out$i; echo err$i >&2; done")
	if want := "out1\nerr1\nout2\nerr2\nout3\nerr3"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRunCommandLongOutputKeepsItsStartAndEnd checks a long output: its first and last whole
// lines come back around a note, and the whole output is saved to the file the note names.
func TestRunCommandLongOutputKeepsItsStartAndEnd(t *testing.T) {
	useCommandLogDir(t)
	got := runCommand(t, "seq 1 30000; echo FAILED: the last test >&2")

	head, rest, ok := strings.Cut(got, "\n\n... [")
	if !ok {
		t.Fatalf("no note in the output:\n%.300s", got)
	}
	note, tail, ok := strings.Cut(rest, "] ...\n\n")
	if !ok {
		t.Fatalf("the note is not closed: %.300s", rest)
	}
	if !strings.HasPrefix(head, "1\n2\n3\n") || len(head) > commandHeadBytes {
		t.Errorf("head: %d bytes starting %.20q; want the first lines within %d bytes", len(head), head, commandHeadBytes)
	}
	if !strings.HasSuffix(tail, "29999\n30000\nFAILED: the last test") || len(tail) > commandTailBytes {
		t.Errorf("tail: %d bytes ending %q; want the last lines within %d bytes", len(tail), tail[max(len(tail)-40, 0):], commandTailBytes)
	}
	assertWholeNumberLines(t, "head", head)
	assertWholeNumberLines(t, "tail", strings.TrimSuffix(tail, "\nFAILED: the last test"))
	if !strings.Contains(note, " omitted; the full output is saved to ") {
		t.Errorf("note = %q", note)
	}

	m := savedLogPath.FindStringSubmatch(note)
	if m == nil {
		t.Fatalf("no saved file in the note %q", note)
	}
	saved, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for i := 1; i <= 30000; i++ {
		fmt.Fprintf(&want, "%d\n", i)
	}
	want.WriteString("FAILED: the last test\n")
	if string(saved) != want.String() {
		t.Errorf("the saved output differs from the command's: %d bytes, want %d", len(saved), want.Len())
	}
}

// assertWholeNumberLines checks that part holds consecutive whole numbers, one per line: the
// cut never falls inside a line.
func assertWholeNumberLines(t *testing.T, name, part string) {
	t.Helper()
	lines := strings.Split(part, "\n")
	var prev int
	for i, l := range lines {
		var n int
		if _, err := fmt.Sscanf(l, "%d", &n); err != nil || fmt.Sprint(n) != l {
			t.Fatalf("%s line %d is not a whole number: %q", name, i, l)
		}
		if i > 0 && n != prev+1 {
			t.Fatalf("%s line %d is %d after %d", name, i, n, prev)
		}
		prev = n
	}
}

// TestCommandOutputCutOnALineStart checks the cut when the shown end begins exactly at a line
// start: that line is kept, not skipped as a partial one.
func TestCommandOutputCutOnALineStart(t *testing.T) {
	o := newCommandOutput("x", nil)
	line := strings.Repeat("a", 63) + "\n" // 64 bytes: both parts hold whole lines exactly
	for range 2000 {
		o.Write([]byte(line))
	}
	got := o.finish()
	head, rest, _ := strings.Cut(got, "\n\n... [")
	_, tail, _ := strings.Cut(rest, "] ...\n\n")
	if n := strings.Count(head, "\n") + 1; n != commandHeadBytes/64 {
		t.Errorf("head has %d lines, want %d", n, commandHeadBytes/64)
	}
	if n := strings.Count(tail, "\n") + 1; n != commandTailBytes/64 {
		t.Errorf("tail has %d lines, want %d", n, commandTailBytes/64)
	}
}

// TestRunCommandLongOutputWithoutSaving checks the note when saving is off: it says how much
// was left out and names no file.
func TestRunCommandLongOutputWithoutSaving(t *testing.T) {
	SetCommandLogDir("")
	got := runCommand(t, "seq 1 30000")
	if !regexp.MustCompile(`\.\.\. \[\d+ KB of \d+ KB omitted\] \.\.\.`).MatchString(got) {
		t.Errorf("want a plain omitted note, got %.200q", got[commandHeadBytes-100:])
	}
}

// TestRunCommandSavedOutputIsCapped checks that a saved output stops at the cap and the note
// says the file holds only the start.
func TestRunCommandSavedOutputIsCapped(t *testing.T) {
	useCommandLogDir(t)
	old := maxCommandLogBytes
	maxCommandLogBytes = 100_000
	t.Cleanup(func() { maxCommandLogBytes = old })

	got := runCommand(t, "seq 1 50000")
	m := savedLogPath.FindStringSubmatch(got)
	if m == nil || !strings.Contains(got, "(its first 97 KB)") {
		t.Fatalf("want a saved file and a note of its cap, got %.300q", got[commandHeadBytes-200:])
	}
	if info, err := os.Stat(m[1]); err != nil || info.Size() != 100_000 {
		t.Errorf("saved file: %v, %v; want 100000 bytes", info, err)
	}
}

// TestCommandLogName checks the saved file's name: the start of the command, in safe
// characters.
func TestCommandLogName(t *testing.T) {
	for cmd, want := range map[string]string{
		"./gradlew :sms:testDebugUnitTest --console=plain": "._gradlew__sms_testDebugUnitTest_--conso",
		"go test ./...": "go_test_._...",
		"$((1+1))":      "1_1",
		"":              "command",
	} {
		if got := commandLogName(cmd); got != want {
			t.Errorf("commandLogName(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// TestRunCommandShortOutputIsValidUTF8 checks an output that holds a broken character, as
// `cut -c` leaves when it cuts inside Arabic text: the broken bytes are dropped.
func TestRunCommandShortOutputIsValidUTF8(t *testing.T) {
	got := runCommand(t, `printf 'ok \331\nnext'`)
	if want := "ok \nnext"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
