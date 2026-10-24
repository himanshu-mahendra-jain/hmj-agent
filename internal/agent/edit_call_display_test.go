package agent

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
