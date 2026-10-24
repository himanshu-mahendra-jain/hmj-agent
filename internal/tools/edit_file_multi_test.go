package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEditTarget writes content to a fresh file for an edit test and returns its path.
func writeEditTarget(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEditFileEditsApplyInOrder checks several edits in one call: each applies to the text the
// previous ones left, replace_all and ignore_whitespace work per edit, and the result counts
// edits and replacements.
func TestEditFileEditsApplyInOrder(t *testing.T) {
	path := writeEditTarget(t, "alpha\n    beta\ngamma gamma\n")
	out, err := (&EditFileTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"path":              path,
		"ignore_whitespace": true,
		"edits": []map[string]any{
			{"old_string": "alpha", "new_string": "ALPHA"},
			{"old_string": "ALPHA", "new_string": "first"}, // sees the first edit's text
			{"old_string": "gamma", "new_string": "g", "replace_all": true},
			{"old_string": "beta", "new_string": "b"}, // matched despite the indent, which it keeps
		},
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(out, "successfully made 4 edits (5 replacement(s)) in "+path) {
		t.Errorf("result = %q", out)
	}
	got, _ := os.ReadFile(path)
	if want := "first\n    b\ng g\n"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// TestEditFileEditsAreAllOrNothing checks that when one of several edits fails, the error
// names it and the file is left as it was.
func TestEditFileEditsAreAllOrNothing(t *testing.T) {
	const content = "one\ntwo\nthree\n"
	for _, tc := range []struct {
		name  string
		edits []map[string]any
		want  []string
	}{
		{"not found", []map[string]any{
			{"old_string": "one", "new_string": "1"},
			{"old_string": "absent", "new_string": "x"},
			{"old_string": "three", "new_string": "3"},
		}, []string{"edit 2 of 3: ", "old_string not found", "no edits were made"}},
		{"gone after an earlier edit", []map[string]any{
			{"old_string": "two", "new_string": "2"},
			{"old_string": "two", "new_string": "deux"},
		}, []string{"edit 2 of 2: ", "no edits were made"}},
		{"ambiguous", []map[string]any{
			{"old_string": "one", "new_string": "t"},
			{"old_string": "t", "new_string": "x"},
		}, []string{"edit 2 of 2: ", "no edits were made"}},
		{"changes nothing and is not found", []map[string]any{
			{"old_string": "one", "new_string": "1"},
			{"old_string": "absent", "new_string": "absent"},
		}, []string{"edit 2 of 2: ", "old_string not found", "no edits were made"}},
		{"empty old_string", []map[string]any{
			{"old_string": "one", "new_string": "1"},
			{"old_string": "", "new_string": "x"},
		}, []string{"edit 2 of 2: old_string cannot be empty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEditTarget(t, content)
			out, err := (&EditFileTool{}).Execute(context.Background(), mustJSON(t, map[string]any{"path": path, "edits": tc.edits}))
			if err == nil {
				t.Fatalf("got %q, want an error", out)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Errorf("file changed to %q", got)
			}
		})
	}
}

// TestEditFileSkipsAnEditThatChangesNothing checks that an edit whose old_string and new_string
// are identical is skipped with a note while the call's other edits apply, and that a call of
// only such edits changes nothing.
func TestEditFileSkipsAnEditThatChangesNothing(t *testing.T) {
	path := writeEditTarget(t, "one\ntwo\nthree\n")
	out, err := (&EditFileTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_string": "two", "new_string": "two"},
			{"old_string": "one", "new_string": "1"},
			{"old_string": "three", "new_string": "three"},
			{"old_string": "1\ntwo", "new_string": "1\n2"},
		},
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(out, "successfully made 2 edits (2 replacement(s)) in "+path) {
		t.Errorf("result = %q", out)
	}
	if !strings.Contains(out, "[note: edits 1 and 3 of 4 changed nothing, as old_string and new_string are identical]") {
		t.Errorf("result lacks the note: %q", out)
	}
	if got, _ := os.ReadFile(path); string(got) != "1\n2\nthree\n" {
		t.Errorf("file = %q", got)
	}

	out, err = (&EditFileTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"path":  path,
		"edits": []map[string]any{{"old_string": "1", "new_string": "1"}, {"old_string": "2", "new_string": "2"}},
	}))
	if err != nil || !strings.HasPrefix(out, "no changes") {
		t.Errorf("all edits identical: %q, %v; want no changes", out, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "1\n2\nthree\n" {
		t.Errorf("file changed to %q", got)
	}
}

// TestEditFileEditsExcludeOldString checks that a call gives either old_string and new_string
// or edits.
func TestEditFileEditsExcludeOldString(t *testing.T) {
	path := writeEditTarget(t, "one\n")
	_, err := (&EditFileTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"path": path, "old_string": "one", "new_string": "1",
		"edits": []map[string]any{{"old_string": "one", "new_string": "2"}},
	}))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("err = %v, want one saying not both", err)
	}
}

// TestPreviewEditMatchesTheEdit checks that PreviewEdit returns the file before and after the
// call as Execute then writes it, and writes nothing itself.
func TestPreviewEditMatchesTheEdit(t *testing.T) {
	const content = "a b a\nkeep\n  c\n"
	path := writeEditTarget(t, content)
	args := mustJSON(t, map[string]any{
		"path":              path,
		"ignore_whitespace": true,
		"edits": []map[string]any{
			{"old_string": "a", "new_string": "x", "replace_all": true},
			{"old_string": "c", "new_string": "  d"},
		},
	})
	before, after, err := PreviewEdit(args)
	if err != nil {
		t.Fatalf("PreviewEdit: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != content || before != content {
		t.Fatalf("before = %q, file = %q; want both %q", before, got, content)
	}
	if _, err := (&EditFileTool{}).Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != after {
		t.Errorf("PreviewEdit's after = %q, Execute wrote %q", after, got)
	}

	if _, _, err := PreviewEdit(mustJSON(t, map[string]any{"path": path, "old_string": "absent", "new_string": "x"})); err == nil {
		t.Error("PreviewEdit of an edit that fails returned no error")
	}
}
