package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInPlaceEditNote checks which commands get the note about sed -i and perl -i edits.
func TestInPlaceEditNote(t *testing.T) {
	ctx := WithScratchDir(context.Background(), "/home/u/.hmj/projects/p/scratch/9")
	for _, tc := range []struct {
		cmd  string
		want string // "" for no note
	}{
		{`sed -i 's/copyNumber(context, it, messages)/copyNumber(context, it)/' src/Entries.kt`, "sed -i"},
		{`cd app && sed -E -i.bak 's/a/b/' x.kt && git diff`, "sed -i.bak"},
		{`sed --in-place 's/a/b/' x.kt`, "sed --in-place"},
		{`perl -pi -e 's/a/b/' x.kt`, "perl -pi"},
		{`./gradlew build | sed -n '1,20p'`, ""},
		{`sed 's/-i/x/' x.kt`, ""},
		{`grep -i foo x.kt | sed 's/a/b/'`, ""},
		{`cd $HMJ_SCRATCH && sed -i 's/a/b/' q.sql`, ""},
		{`sed -i 's/a/b/' /home/u/.hmj/projects/p/scratch/9/q.sql`, ""},
	} {
		got := inPlaceEditNote(ctx, tc.cmd)
		if tc.want == "" {
			if got != "" {
				t.Errorf("%s: got %q, want no note", tc.cmd, got)
			}
			continue
		}
		if !strings.HasPrefix(got, "[note: "+tc.want+" changes files with no diff shown") {
			t.Errorf("%s: got %q, want a note naming %q", tc.cmd, got, tc.want)
		}
	}
}

// TestRunCommandNotesAnInPlaceEdit checks that the note follows the command's own result,
// after a success with no output and after an exit code.
func TestRunCommandNotesAnInPlaceEdit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(f, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &RunCommandTool{}
	out, _ := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "sed -i 's/a/b/' " + f}))
	if !strings.HasPrefix(out, "[command executed successfully with no output]\n[note: sed -i ") {
		t.Errorf("success: %q", out)
	}
	out, _ = tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "sed -i 's/a/b/' " + f + "; exit 3"}))
	if !strings.HasPrefix(out, "[Exit code 3]\n[note: sed -i ") {
		t.Errorf("failure: %q", out)
	}
}
