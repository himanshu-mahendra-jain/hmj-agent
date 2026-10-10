package tools

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useBackupDir(t *testing.T, dir string) {
	t.Helper()
	SetBackupDir(dir)
	t.Cleanup(func() { SetBackupDir("") })
}

func backups(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got[e.Name()] = string(data)
	}
	return got
}

func runTool(t *testing.T, tool Tool, args map[string]any) string {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), string(argsJSON))
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return out
}

// edit_file and write_file keep the previous content of a file they change; a new
// file has nothing to back up.
func TestEditsAreBackedUp(t *testing.T) {
	work, store := t.TempDir(), filepath.Join(t.TempDir(), "backups")
	useBackupDir(t, store)
	target := filepath.Join(work, "notes.txt")
	if err := os.WriteFile(target, []byte("version one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runTool(t, &EditFileTool{}, map[string]any{"path": target, "old_string": "one", "new_string": "two"})
	runTool(t, &WriteFileTool{}, map[string]any{"path": target, "content": "version three\n"})
	runTool(t, &WriteFileTool{}, map[string]any{"path": filepath.Join(work, "new.txt"), "content": "fresh"})

	got := backups(t, store)
	if len(got) != 2 {
		t.Fatalf("got %d backups %v, want 2", len(got), got)
	}
	var contents []string
	for name, content := range got {
		if !strings.HasSuffix(name, "_"+strings.ReplaceAll(strings.TrimPrefix(target, "/"), "/", "%")) {
			t.Errorf("backup name %q does not encode %s", name, target)
		}
		contents = append(contents, content)
	}
	if strings.Join(contents, "") != "version one\nversion two\n" && strings.Join(contents, "") != "version two\nversion one\n" {
		t.Errorf("backups hold %q, want the content before each change", contents)
	}
}

func TestNoBackupsByDefault(t *testing.T) {
	target := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(target, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runTool(t, &WriteFileTool{}, map[string]any{"path": target, "content": "b"}); strings.Contains(out, "backup") {
		t.Errorf("unexpected backup note: %q", out)
	}
}

// Only the newest maxBackups backups are kept.
func TestBackupsArePruned(t *testing.T) {
	store := t.TempDir()
	useBackupDir(t, store)
	for i := range maxBackups + 5 {
		if err := os.WriteFile(filepath.Join(store, fmt.Sprintf("20000101-000000.%06d_old", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(target, []byte("latest"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTool(t, &WriteFileTool{}, map[string]any{"path": target, "content": "new"})

	got := backups(t, store)
	if len(got) != maxBackups {
		t.Errorf("%d backups kept, want %d", len(got), maxBackups)
	}
	if _, ok := got["20000101-000000.000000_old"]; ok {
		t.Error("the oldest backup was kept")
	}
	found := false
	for _, content := range got {
		found = found || content == "latest"
	}
	if !found {
		t.Error("the newest backup was pruned")
	}
}

// A backup that cannot be written is reported but does not block the edit.
func TestBackupFailureDoesNotBlockEdit(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	useBackupDir(t, filepath.Join(blocker, "backups"))
	target := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runTool(t, &EditFileTool{}, map[string]any{"path": target, "old_string": "old", "new_string": "new"})
	if !strings.Contains(out, "no backup of the previous content was saved") {
		t.Errorf("missing backup warning: %q", out)
	}
	if data, _ := os.ReadFile(target); string(data) != "new" {
		t.Errorf("edit not applied: %q", data)
	}
}
