package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Less common .gitignore syntax: escapes, trailing spaces, empty and repeated
// segments, a trailing "/**", a bare "**", and lines that hold no pattern at all.
func TestGitIgnorePatternEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		isDir   bool
		ignored bool
	}{
		{"escaped bracket is literal", `\[!x]y`, "[!x]y", false, true},
		{"escaped bracket does not negate", `\[!x]y`, "ay", false, false},
		{"unescaped trailing spaces are dropped", "notes.txt   ", "notes.txt", false, true},
		{"empty segment is dropped", "a//b", "a/b", false, true},
		{"repeated ** collapses", "**/**/target", "x/y/target", true, true},
		{"repeated ** matches at the top", "**/**/target", "target", true, true},
		{"trailing /** matches contents", "logs/**", "logs/2024/app.log", false, true},
		{"trailing /** does not match the directory itself", "logs/**", "logs", true, false},
		{"bare ** matches everything", "**", "any/where/file.go", false, true},
		{"lone ! is not a pattern", "!", "!", false, false},
		{"lone / is not a pattern", "/", "x", false, false},
		{"// is not a pattern", "//", "x", true, false},
		{"comment", "# x", "# x", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newGitIgnoreMatcher()
			m.addRules("", tc.pattern)
			if got := m.Match(tc.path, tc.isDir); got != tc.ignored {
				t.Errorf("pattern %q: Match(%q, dir=%v) = %v, want %v", tc.pattern, tc.path, tc.isDir, got, tc.ignored)
			}
		})
	}
}

// Rules from a subdirectory's .gitignore apply below that directory, never to the
// directory itself or to its siblings.
func TestGitIgnoreRulesScopedToTheirDirectory(t *testing.T) {
	m := newGitIgnoreMatcher()
	m.addRules("sub", "*")
	tests := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		{"sub", true, false},
		{"sub/x.txt", false, true},
		{"subway/x.txt", false, false},
		{"other", true, false},
	}
	for _, tc := range tests {
		if got := m.Match(tc.path, tc.isDir); got != tc.ignored {
			t.Errorf("Match(%q) = %v, want %v", tc.path, got, tc.ignored)
		}
	}
}

// writeTree creates files (and their parent directories) under the current directory.
func writeTree(t *testing.T, files ...string) {
	t.Helper()
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		content := ""
		if strings.HasSuffix(f, ".gitignore") {
			content = "*.tmp\n"
		}
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func listFiles(t *testing.T, args string) (string, error) {
	t.Helper()
	return (&ListFilesTool{}).Execute(context.Background(), args)
}

// A non-recursive listing shows only the top level; a .gitignore found during the walk
// applies inside its own directory only.
func TestListFilesNonRecursiveAndNestedGitIgnore(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTree(t, "top.tmp", "a/inner.txt", "a/deeper/x.txt", "sub/.gitignore", "sub/drop.tmp", "sub/keep.txt")

	out, err := listFiles(t, `{"path":".","recursive":false}`)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(out, "\n")
	want := []string{"a/", "sub/", "top.tmp"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("non-recursive listing = %q, want %q", got, want)
	}

	out, err = listFiles(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"top.tmp", "sub/keep.txt", "sub/.gitignore", "a/deeper/x.txt"} {
		if !strings.Contains(out, w) {
			t.Errorf("recursive listing misses %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "drop.tmp") {
		t.Errorf("sub/.gitignore did not hide sub/drop.tmp:\n%s", out)
	}
}

// The listing stops at listFilesMaxResults entries and says so.
func TestListFilesTruncatesAtLimit(t *testing.T) {
	t.Chdir(t.TempDir())
	for i := range listFilesMaxResults + 5 {
		if err := os.WriteFile(fmt.Sprintf("f%03d", i), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := listFiles(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != listFilesMaxResults+1 || lines[len(lines)-1] != fmt.Sprintf("[truncated at %d entries]", listFilesMaxResults) {
		t.Errorf("got %d lines ending %q, want %d entries and the truncation note", len(lines), lines[len(lines)-1], listFilesMaxResults)
	}
}

// A missing path is an error; an empty directory is "no files found".
func TestListFilesMissingAndEmpty(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := listFiles(t, `{"path":"nope"}`); err == nil || !strings.Contains(err.Error(), "path not found: nope") {
		t.Errorf("missing path: err = %v", err)
	}
	if err := os.Mkdir("empty", 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := listFiles(t, `{"path":"empty"}`); err != nil || out != "no files found" {
		t.Errorf("empty dir: %q, %v", out, err)
	}
}

// An unreadable subdirectory is skipped and the rest is listed; an unreadable target
// directory is an error.
func TestListFilesUnreadableDirectories(t *testing.T) {
	skipIfRoot(t)
	t.Chdir(t.TempDir())
	writeTree(t, "open/a.txt", "locked/b.txt")
	if err := os.Chmod("locked", 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod("locked", 0o755) })

	out, err := listFiles(t, `{}`)
	if err != nil {
		t.Fatalf("unreadable subdirectory failed the listing: %v", err)
	}
	if !strings.Contains(out, "open/a.txt") || strings.Contains(out, "b.txt") {
		t.Errorf("listing = %q, want open/a.txt and nothing from locked/", out)
	}

	if _, err := listFiles(t, `{"path":"locked"}`); err == nil || !strings.Contains(err.Error(), "failed to list files") {
		t.Errorf("unreadable target: err = %v, want failed to list files", err)
	}
}
