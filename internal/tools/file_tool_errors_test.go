package tools

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skipIfRoot skips tests that rely on file permissions, which root ignores.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced for root")
	}
}

// unreadableFile writes a file and removes every permission from it.
func unreadableFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(p, []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o600) })
	return p
}

// Every tool rejects malformed JSON and missing required arguments with an error rather
// than acting on zero values.
func TestToolsRejectBadArguments(t *testing.T) {
	tests := []struct {
		name    string
		tool    Tool
		args    string
		wantErr string
	}{
		{"read_file malformed", &ReadFileTool{}, `{"path":`, "invalid arguments"},
		{"read_file no path", &ReadFileTool{}, `{}`, "path is required"},
		{"write_file malformed", &WriteFileTool{}, `[1]`, "invalid arguments"},
		{"write_file no path", &WriteFileTool{}, `{"content":"x"}`, "path is required"},
		{"edit_file malformed", &EditFileTool{}, `nope`, "invalid arguments"},
		{"edit_file no old_string", &EditFileTool{}, `{"path":"a.txt","new_string":"x"}`, "old_string cannot be empty"},
		{"run_command malformed", &RunCommandTool{}, `{"command":1}`, "invalid arguments"},
		{"run_command blank command", &RunCommandTool{}, `{"command":"  \t"}`, "command is required"},
		{"fetch_url malformed", &FetchURLTool{}, `{`, "invalid arguments"},
		{"fetch_url no url", &FetchURLTool{}, `{"url":"  "}`, "url is required"},
		{"web_search malformed", &WebSearchTool{}, `{"query":[]}`, "invalid arguments"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.tool.Execute(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Execute(%s) = %q, %v; want error containing %q", tc.args, out, err, tc.wantErr)
			}
		})
	}
}

// A "~" path cannot be resolved without a home directory; every path-taking tool
// reports that instead of guessing a location.
func TestToolsReportUnresolvableHome(t *testing.T) {
	t.Setenv("HOME", "")
	tests := []struct {
		tool Tool
		args string
	}{
		{&ReadFileTool{}, `{"path":"~/a.txt"}`},
		{&WriteFileTool{}, `{"path":"~/a.txt","content":"x"}`},
		{&EditFileTool{}, `{"path":"~/a.txt","old_string":"a","new_string":"b"}`},
		{&ListFilesTool{}, `{"path":"~"}`},
		{&RunCommandTool{}, `{"command":"true","workdir":"~"}`},
	}
	for _, tc := range tests {
		t.Run(tc.tool.Name(), func(t *testing.T) {
			_, err := tc.tool.Execute(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), "home directory") {
				t.Errorf("err = %v, want a home directory error", err)
			}
		})
	}
}

// read_file refuses what it cannot or should not load and describes empty files and
// offsets past the end instead of returning nothing.
func TestReadFileEdgeCases(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(dir, "huge.bin")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxReadableFileSize + 1); err != nil { // sparse: no disk cost
		t.Fatal(err)
	}
	f.Close()
	short := filepath.Join(dir, "short.txt")
	if err := os.WriteFile(short, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    map[string]any
		want    string // exact output, when no error is expected
		wantErr string
	}{
		{"directory", map[string]any{"path": dir}, "", "is a directory"},
		{"over the size cap", map[string]any{"path": huge}, "", "too large"},
		{"missing", map[string]any{"path": filepath.Join(dir, "nope")}, "", "no such file"},
		{"empty file", map[string]any{"path": empty}, "[empty file]", ""},
		{"offset past the end", map[string]any{"path": short, "offset": 5}, "file has 2 lines (requested offset 5)", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&ReadFileTool{}).Execute(context.Background(), mustJSON(t, tc.args))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("got %q, %v; want error containing %q", out, err, tc.wantErr)
				}
				return
			}
			if err != nil || out != tc.want {
				t.Errorf("got %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

// read_file passes on the permission error of a file it may not open.
func TestReadFileUnreadable(t *testing.T) {
	skipIfRoot(t)
	p := unreadableFile(t, t.TempDir())
	if _, err := (&ReadFileTool{}).Execute(context.Background(), mustJSON(t, map[string]any{"path": p})); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want permission denied", err)
	}
}

// A first line of exactly MaxReadFileBytes fits the cap, so it is returned whole and the
// trailer offers the next line instead of claiming the line was cut.
func TestReadFileLineExactlyAtByteCapIsNotReportedCut(t *testing.T) {
	line := strings.Repeat("z", MaxReadFileBytes)
	path := writeReadFixture(t, line+"\nsecond\n")
	out := readWith(t, path, 0, 0)
	if strings.Contains(out, "was cut") {
		t.Fatalf("a line that fits was reported as cut: %q", out[len(line):])
	}
	if !strings.HasPrefix(out, line+"\n") {
		t.Errorf("line not returned whole")
	}
	if want := "--- truncated: showing lines 1-1 of 2; call read_file again with offset=2 ---"; !strings.HasSuffix(out, want) {
		t.Errorf("trailer = %q, want %q", out[len(line):], want)
	}
}

// An overlong first line followed by more lines is cut, and the note says where to go on.
func TestReadFileCutLineOffersContinuation(t *testing.T) {
	path := writeReadFixture(t, strings.Repeat("q", MaxReadFileBytes+10)+"\nnext\n")
	out := readWith(t, path, 0, 0)
	if want := "was cut; call read_file again with offset=2 to continue ---"; !strings.HasSuffix(out, want) {
		t.Errorf("tail = %q, want suffix %q", out[max(0, len(out)-120):], want)
	}
}

// write_file reports where it could not write: a parent that is a file, or a target that
// is a directory.
func TestWriteFileFailures(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, path, wantErr string
	}{
		{"parent is a file", filepath.Join(blocker, "child.txt"), "failed to create directory"},
		{"target is a directory", dir, "failed to write"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&WriteFileTool{}).Execute(context.Background(), mustJSON(t, map[string]any{"path": tc.path, "content": "y"}))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// edit_file explains why an edit did not happen, and an edit that changes nothing does
// not touch the file.
func TestEditFileOutcomes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	const content = "func main() {\n\tprintln(\"hi\")\n}\n"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		args     map[string]any
		wantOut  string
		wantErrs []string
	}{
		{"identical strings", map[string]any{"path": file, "old_string": "main", "new_string": "main"}, "no changes: old_string and new_string are identical", nil},
		{"directory", map[string]any{"path": dir, "old_string": "a", "new_string": "b"}, "", []string{"is a directory"}},
		{"missing file", map[string]any{"path": filepath.Join(dir, "nope.go"), "old_string": "a", "new_string": "b"}, "", []string{"no such file"}},
		{"more lines than the file", map[string]any{"path": file, "old_string": "a\nb\nc\nd\ne\nf", "new_string": "x"}, "", []string{"old_string not found in", "a.go"}},
		{"whitespace collapsed across lines", map[string]any{"path": file, "old_string": "func main() { println(\"hi\")", "new_string": "x"}, "", []string{"near match exists with different whitespace", "smaller 3-5 line unique anchor"}},
		{"nothing similar", map[string]any{"path": file, "old_string": "absent text", "new_string": "x"}, "", []string{"old_string not found in " + file}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&EditFileTool{}).Execute(context.Background(), mustJSON(t, tc.args))
			if tc.wantErrs == nil {
				if err != nil || out != tc.wantOut {
					t.Errorf("got %q, %v; want %q", out, err, tc.wantOut)
				}
			} else {
				if err == nil {
					t.Fatalf("got %q, want an error", out)
				}
				for _, w := range tc.wantErrs {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not contain %q", err, w)
					}
				}
			}
			if got, _ := os.ReadFile(file); string(got) != content {
				t.Errorf("file changed to %q", got)
			}
		})
	}
}

// edit_file passes on permission errors: it cannot read an unreadable file, and it
// cannot save over a read-only one (which it leaves unchanged).
func TestEditFilePermissionErrors(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	p := unreadableFile(t, dir)
	args := mustJSON(t, map[string]any{"path": p, "old_string": "hidden", "new_string": "shown"})
	if _, err := (&EditFileTool{}).Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("unreadable: err = %v, want permission denied", err)
	}

	ro := filepath.Join(dir, "ro.txt")
	if err := os.WriteFile(ro, []byte("hello"), 0o400); err != nil {
		t.Fatal(err)
	}
	args = mustJSON(t, map[string]any{"path": ro, "old_string": "hello", "new_string": "bye"})
	if _, err := (&EditFileTool{}).Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "failed to save") {
		t.Errorf("read-only: err = %v, want failed to save", err)
	}
	if got, _ := os.ReadFile(ro); string(got) != "hello" {
		t.Errorf("read-only file changed to %q", got)
	}
}

// When the backup directory cannot be created or written, the edit still happens and
// the result carries a warning.
func TestBackupDirectoryFailuresWarn(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly := filepath.Join(root, "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(readOnly, 0o700) })

	for name, backupDir := range map[string]string{
		"backup dir under a file": filepath.Join(blocker, "backups"),
		"read-only backup dir":    readOnly,
	} {
		t.Run(name, func(t *testing.T) {
			useBackupDir(t, backupDir)
			target := filepath.Join(t.TempDir(), "t.txt")
			if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			out := runTool(t, &WriteFileTool{}, map[string]any{"path": target, "content": "new"})
			if !strings.Contains(out, "warning: no backup of the previous content was saved") {
				t.Errorf("no backup warning in %q", out)
			}
			if got, _ := os.ReadFile(target); string(got) != "new" {
				t.Errorf("file = %q, want the new content", got)
			}
		})
	}
}

// Pruning skips anything in the backup directory that is not a regular file.
func TestBackupPruneIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	useBackupDir(t, dir)
	if err := os.Mkdir(filepath.Join(dir, "99999999-subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "t.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTool(t, &WriteFileTool{}, map[string]any{"path": target, "content": "new"})
	if _, err := os.Stat(filepath.Join(dir, "99999999-subdir")); err != nil {
		t.Errorf("pruning removed a directory: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("backup dir has %d entries, want the subdir and one backup", len(entries))
	}
}

// mustJSON encodes tool arguments.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A backup of a deeply nested file keeps the end of the encoded path, the part that
// names the file, so the backup name stays under the file name length limit.
func TestBackupNameOfLongPathKeepsItsEnd(t *testing.T) {
	store := t.TempDir()
	useBackupDir(t, store)
	deep := t.TempDir()
	for range 12 {
		deep = filepath.Join(deep, "a-rather-long-directory-name")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(deep, "final-file.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTool(t, &WriteFileTool{}, map[string]any{"path": target, "content": "new"})
	got := backups(t, store)
	if len(got) != 1 {
		t.Fatalf("got %d backups, want 1", len(got))
	}
	for name, content := range got {
		if len(name) > 255 || !strings.HasSuffix(name, "%final-file.txt") || content != "old" {
			t.Errorf("backup %q (%d bytes) = %q; want a short name ending in the file name", name, len(name), content)
		}
	}
}
