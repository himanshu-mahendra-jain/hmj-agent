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

// editCallOutput returns what the transcript shows for an edit_file call with args.
func editCallOutput(t *testing.T, args map[string]any) string {
	t.Helper()
	theme.SetEnabled(false)
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return captureOutput(t, func() { printExpandedToolCall("edit_file", string(b)) })
}

// TestEditCallShowsEveryEdit checks the diff shown for an edit_file call with several edits:
// one diff of the whole call, with each change as the tool will make it, including one that
// applies to an earlier edit's text and a replace_all.
func TestEditCallShowsEveryEdit(t *testing.T) {
	var content strings.Builder
	for i := range 60 {
		content.WriteString("line " + strings.Repeat("x", i%3) + "\n")
	}
	content.WriteString("top\n")
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("first\n"+content.String()+"last\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := editCallOutput(t, map[string]any{"path": path, "edits": []map[string]any{
		{"old_string": "first", "new_string": "FIRST"},
		{"old_string": "FIRST", "new_string": "opening"},
		{"old_string": "last", "new_string": "closing"},
		{"old_string": "top", "new_string": "summit", "replace_all": true},
	}})
	for _, want := range []string{"(4 edits)", "opening", "closing", "summit"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "FIRST") {
		t.Errorf("shows an intermediate edit's text rather than the result:\n%s", out)
	}
}

// TestEditCallShowsReplaceAll checks that a single edit with replace_all shows every
// occurrence replaced, not just the first.
func TestEditCallShowsReplaceAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("cat\ndog\ncat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := editCallOutput(t, map[string]any{"path": path, "old_string": "cat", "new_string": "lion", "replace_all": true})
	if n := strings.Count(out, "lion"); n != 2 {
		t.Errorf("shows %d replacements, want 2:\n%s", n, out)
	}
}

// TestEditCallThatWillFailShowsEachEdit checks the fallback when the edits cannot apply: each
// one shows as minus/plus lines, its text sanitized.
func TestEditCallThatWillFailShowsEachEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := editCallOutput(t, map[string]any{"path": path, "edits": []map[string]any{
		{"old_string": "one", "new_string": "1"},
		{"old_string": "absent", "new_string": "x\x1b]52;c;Zm9v\x07y"},
	}})
	for _, want := range []string{"(2 edits)", "absent", "xy"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%q", want, out)
		}
	}
	assertNoInjectedControls(t, out)
}

// TestResumedEditCallShowsNoDiff checks that an edit_file call in a resumed transcript shows its
// old and new text as written, not a diff against the file as it is now: an insertion whose
// anchor is still there would otherwise be applied a second time and shown as a duplicate.
func TestResumedEditCallShowsNoDiff(t *testing.T) {
	theme.SetEnabled(false)
	path := filepath.Join(t.TempDir(), "imports.kt")
	if err := os.WriteFile(path, []byte("import a.remember\nimport a.rememberSaveable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(map[string]any{"path": path, "old_string": "import a.remember\n", "new_string": "import a.remember\nimport a.rememberSaveable\n"})
	if err != nil {
		t.Fatal(err)
	}
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "add the import"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "e1", Type: "function", Function: chat.ToolCallFunction{Name: "edit_file", Arguments: string(args)}}}},
		{Role: "tool", ToolCallID: "e1", Content: "successfully replaced 1 occurrence(s) in " + path},
	}}
	out := captureOutput(t, func() { PrintConversationHistory(conv) })
	if strings.Contains(out, "+1 lines") || strings.Contains(out, "3 + ") {
		t.Errorf("a resumed edit_file call shows a diff against the file as it is now:\n%s", out)
	}
	for _, want := range []string{"edit_file: " + path + "\n", "-  import a.remember", "+ import a.rememberSaveable"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// TestEditCallCountsOneEdit checks that an edits list of one is not counted as "1 edits".
func TestEditCallCountsOneEdit(t *testing.T) {
	out := editCallOutput(t, map[string]any{"path": filepath.Join(t.TempDir(), "absent.txt"), "edits": []map[string]any{{"old_string": "a", "new_string": "b"}}})
	if !strings.Contains(out, "(1 edit)") {
		t.Errorf("no \"(1 edit)\" in:\n%s", out)
	}
}
