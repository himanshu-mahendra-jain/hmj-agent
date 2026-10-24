package agent

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
)

// writeCallArgs returns the JSON arguments of a write_file call.
func writeCallArgs(t *testing.T, path, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"path": path, "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeCallOutput returns what the transcript shows for a write_file call, printed as it is
// before the tool runs.
func writeCallOutput(t *testing.T, path, content string) string {
	t.Helper()
	theme.SetEnabled(false)
	return captureOutput(t, func() { printExpandedToolCall("write_file", writeCallArgs(t, path, content)) })
}

// TestWriteCallShowsTheDiffOfAnOverwrite checks that overwriting a file shows the lines removed
// and added against its current content, and leaves unchanged lines as context.
func TestWriteCallShowsTheDiffOfAnOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fruits.txt")
	if err := os.WriteFile(path, []byte("Apple\nBanana\nOrange\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := writeCallOutput(t, path, "Blueberry\nBanana\nPlum\n")
	for _, want := range []string{"write_file: " + path + " (22 bytes)", "+2 / -2 lines", "- Apple", "+ Blueberry", "  Banana", "- Orange", "+ Plum"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "- Banana") || strings.Contains(out, "+ Banana") {
		t.Errorf("an unchanged line is shown as changed:\n%s", out)
	}
}

// TestWriteCallShowsANewFileAsAdded checks that a write_file call creating a file shows every
// line as added, the last one included when it has no line break.
func TestWriteCallShowsANewFileAsAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "new.txt")
	out := writeCallOutput(t, path, "one\ntwo")
	for _, want := range []string{"+2 lines", "1 + one", "2 + two"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, " - ") {
		t.Errorf("a new file shows removed lines:\n%s", out)
	}
}

// TestWriteCallSaysWhenNothingChanges checks that writing a file's own content shows a note in
// place of an empty diff.
func TestWriteCallSaysWhenNothingChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "same.txt")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := writeCallOutput(t, path, "same\n")
	if !strings.Contains(out, "[no change: the file already has this content]") {
		t.Errorf("no unchanged note in:\n%s", out)
	}
}

// TestWriteCallShowsNoDiffForAFileTooLargeToBackUp checks that a file over write_file's size limit
// is not read for a diff.
func TestWriteCallShowsNoDiffForAFileTooLargeToBackUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(11 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out := writeCallOutput(t, path, "small\n")
	if strings.Contains(out, "lines") || strings.Contains(out, "+ small") {
		t.Errorf("a diff is shown for a file over the limit:\n%s", out)
	}
}

// TestResumedWriteCallShowsNoDiff checks that a write_file call in a resumed transcript shows its
// call line only: the file already holds what the call wrote, so a diff against it would be wrong.
func TestResumedWriteCallShowsNoDiff(t *testing.T) {
	theme.SetEnabled(false)
	path := filepath.Join(t.TempDir(), "fruits.txt")
	if err := os.WriteFile(path, []byte("Plum\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "replace the fruits"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "w1", Type: "function", Function: chat.ToolCallFunction{
			Name: "write_file", Arguments: writeCallArgs(t, path, "Plum\n"),
		}}}},
		{Role: "tool", ToolCallID: "w1", Content: "successfully wrote 5 bytes to " + path},
	}}
	out := captureOutput(t, func() { PrintConversationHistory(conv) })
	if !strings.Contains(out, "write_file: "+path+" (5 bytes)") {
		t.Errorf("no call line in:\n%s", out)
	}
	if strings.Contains(out, "no change") || strings.Contains(out, "+ Plum") {
		t.Errorf("a resumed write_file call shows a diff against the file as it is now:\n%s", out)
	}
}
