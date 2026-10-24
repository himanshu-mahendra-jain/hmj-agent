package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadFileNumbersLinesByDefault checks that lines come numbered unless the call turns the
// numbers off, and that numbers follow the offset.
func TestReadFileNumbersLinesByDefault(t *testing.T) {
	path := writeReadFixture(t, "alpha\n\tbeta\ngamma\n")
	if got, want := readArgs(t, map[string]any{"path": path}), "   1 | alpha\n   2 | \tbeta\n   3 | gamma"; got != want {
		t.Errorf("default: got %q, want %q", got, want)
	}
	if got, want := readArgs(t, map[string]any{"path": path, "numbered": false}), "alpha\n\tbeta\ngamma"; got != want {
		t.Errorf("numbered false: got %q, want %q", got, want)
	}
	got := readArgs(t, map[string]any{"path": path, "offset": 2, "limit": 1})
	if want := "   2 | \tbeta"; !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "--- ") {
		t.Errorf("offset 2: got %q, want a range header and %q", got, want)
	}
}

// TestReadFileOfADirectoryListsIt checks that read_file given a directory lists its entries,
// subdirectories two levels down and their files, instead of failing.
func TestReadFileOfADirectoryListsIt(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFiles(t, map[string]string{"pkg/a.go": "", "pkg/sub/b.go": "", "pkg/sub/deep/c.go": "", "top.txt": ""})
	got := readArgs(t, map[string]any{"path": "."})
	if !strings.HasPrefix(got, ". is a directory;") {
		t.Errorf("no directory note: %q", got)
	}
	for _, want := range []string{"\npkg/\n", "\npkg/a.go\n", "\npkg/sub/\n", "\npkg/sub/b.go\n", "\ntop.txt"} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("no %q in %q", strings.TrimSpace(want), got)
		}
	}
	if strings.Contains(got, "pkg/sub/deep") {
		t.Errorf("lists past two levels: %q", got)
	}
}

// TestEditFileSaysWhenOldStringHasLineNumbers checks the error for an old_string copied from
// read_file's numbered output.
func TestEditFileSaysWhenOldStringHasLineNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("func a() {\n\treturn\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (&EditFileTool{}).Execute(context.Background(), mustJSON(t, map[string]any{
		"path": path, "old_string": "   1 | func a() {\n   2 | \treturn", "new_string": "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "leave them out of old_string") {
		t.Errorf("err = %v, want the line-number hint", err)
	}
}

// TestListFilesPattern checks list_files' pattern: only the files it matches, at any depth
// unless max_depth is given, and no directories.
func TestListFilesPattern(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFiles(t, map[string]string{
		"app/src/main/Handler.kt":             "",
		"app/src/test/a/b/c/HandlerTest.kt":   "",
		"app/src/test/a/b/c/HandlerTest.java": "",
		"lib/src/test/UtilTest.kt":            "",
		"README.md":                           "",
	})
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"pattern": "*Test.kt"}, "app/src/test/a/b/c/HandlerTest.kt\nlib/src/test/UtilTest.kt"},
		{map[string]any{"pattern": "app/**/*Test.*"}, "app/src/test/a/b/c/HandlerTest.java\napp/src/test/a/b/c/HandlerTest.kt"},
		{map[string]any{"path": "lib", "pattern": "src/**/*.kt"}, "src/test/UtilTest.kt"},
		{map[string]any{"pattern": "*.kt", "max_depth": 4}, "app/src/main/Handler.kt\nlib/src/test/UtilTest.kt"},
		{map[string]any{"pattern": "*.swift"}, "no files found"},
	} {
		got, err := listFiles(t, mustJSON(t, tc.args))
		if err != nil || got != tc.want {
			t.Errorf("%v: got %q, %v; want %q", tc.args, got, err, tc.want)
		}
	}
	if _, err := listFiles(t, `{"pattern":"[a"}`); err == nil || !strings.Contains(err.Error(), "invalid glob") {
		t.Errorf("bad pattern: err = %v", err)
	}
}
