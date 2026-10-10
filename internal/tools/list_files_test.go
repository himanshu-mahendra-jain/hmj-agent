package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitIgnoreMatcher_Unit(t *testing.T) {
	matcher := newGitIgnoreMatcher()
	matcher.addRules("", strings.Join([]string{
		"# Comments and blank lines",
		"",
		"*.log",
		"!important.log",
		"/root_only.txt",
		"build/",
		"doc/**/*.txt",
		"**/deep/*.bin",
		"a/**/b",
		"trailing_space\\ ",
		"\\#hashfile",
		"\\!exclfile",
		"[!0-9].dat",
	}, "\n"))

	tests := []struct {
		path    string
		isDir   bool
		ignored bool
		desc    string
	}{
		{"app.log", false, true, "wildcard *.log file"},
		{"sub/app.log", false, true, "wildcard *.log in subdirectory"},
		{"important.log", false, false, "negated !important.log"},
		{"sub/important.log", false, false, "negated !important.log in subdirectory"},
		{"root_only.txt", false, true, "anchored /root_only.txt at root"},
		{"sub/root_only.txt", false, false, "anchored /root_only.txt in sub should not match"},
		{"build", true, true, "directory-only build/ matches directory"},
		{"build", false, false, "directory-only build/ does not match file"},
		{"sub/build", true, true, "unanchored directory-only build/ in sub matches directory"},
		{"doc/readme.txt", false, true, "doc/**/*.txt matches direct child"},
		{"doc/sub/nested/readme.txt", false, true, "doc/**/*.txt matches nested child"},
		{"other/readme.txt", false, false, "doc/**/*.txt does not match outside doc"},
		{"deep/foo.bin", false, true, "**/deep/*.bin matches direct deep"},
		{"x/y/deep/foo.bin", false, true, "**/deep/*.bin matches nested deep"},
		{"deep/foo.txt", false, false, "**/deep/*.bin requires .bin suffix"},
		{"a/b", false, true, "a/**/b matches zero intermediate directories"},
		{"a/x/y/b", false, true, "a/**/b matches multiple intermediate directories"},
		{"x/a/b", false, false, "a/**/b is anchored to baseDir"},
		{"trailing_space ", false, true, "escaped trailing space"},
		{"trailing_space", false, false, "without trailing space should not match"},
		{"#hashfile", false, true, "escaped leading hash"},
		{"!exclfile", false, true, "escaped leading exclamation"},
		{"a.dat", false, true, "negated character class [!0-9] matches non-digit"},
		{"1.dat", false, false, "negated character class [!0-9] does not match digit"},
		{"unrelated.go", false, false, "unrelated file is not ignored"},
	}

	for _, tc := range tests {
		got := matcher.Match(tc.path, tc.isDir)
		if got != tc.ignored {
			t.Errorf("[%s] Match(%q, %v) = %v; want %v", tc.desc, tc.path, tc.isDir, got, tc.ignored)
		}
	}
}

func TestGitIgnoreMatcher_SubdirectoryScoping(t *testing.T) {
	matcher := newGitIgnoreMatcher()
	// Root ignores *.txt except notes.txt
	matcher.addRules("", "*.txt\n!notes.txt")
	// Subdirectory sub1 overrides notes.txt
	matcher.addRules("sub1", "notes.txt\n!sub1_custom.txt")

	if !matcher.Match("file.txt", false) {
		t.Errorf("expected file.txt to be ignored by root *.txt")
	}
	if matcher.Match("notes.txt", false) {
		t.Errorf("expected notes.txt to be kept by root !notes.txt")
	}
	if !matcher.Match("sub1/notes.txt", false) {
		t.Errorf("expected sub1/notes.txt to be ignored by sub1/notes.txt override")
	}
	if matcher.Match("sub1/sub1_custom.txt", false) {
		t.Errorf("expected sub1/sub1_custom.txt to be kept by sub1/!sub1_custom.txt")
	}
	if !matcher.Match("sub1/other.txt", false) {
		t.Errorf("expected sub1/other.txt to be ignored by root *.txt")
	}
}

func TestListFilesTool_NoExclusionsExceptGit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	dirs := []string{
		".git",
		"node_modules",
		"vendor",
		"dist",
		"build",
		".vscode",
		"src",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	files := map[string]string{
		".git/HEAD":                 "ref: refs/heads/main",
		"node_modules/pkg/index.js": "console.log(1)",
		"vendor/dep/dep.go":         "package dep",
		"dist/bundle.js":            "var a=1",
		"build/output.bin":          "bin",
		".vscode/settings.json":     "{}",
		".DS_Store":                 "junk",
		"src/app.min.js":            "min",
		"src/main.go":               "package main",
	}
	for p, content := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &ListFilesTool{}
	ctx := context.Background()

	// Without .gitignore: ONLY .git must be excluded!
	res, err := tool.Execute(ctx, `{"path":".","recursive":true,"max_depth":4}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(res, ".git") {
		t.Errorf(".git must be excluded, got:\n%s", res)
	}

	// Verify all other folders and files are NOT excluded
	expectedPresent := []string{
		"node_modules/",
		"node_modules/pkg/index.js",
		"vendor/",
		"vendor/dep/dep.go",
		"dist/",
		"dist/bundle.js",
		"build/",
		"build/output.bin",
		".vscode/",
		".vscode/settings.json",
		".DS_Store",
		"src/app.min.js",
		"src/main.go",
	}
	for _, exp := range expectedPresent {
		if !strings.Contains(res, exp) {
			t.Errorf("expected %q to be present in output, got:\n%s", exp, res)
		}
	}

	// Now add .gitignore ignoring node_modules, dist, and .DS_Store
	gitignoreContent := "node_modules/\ndist/\n.DS_Store\n"
	if err := os.WriteFile(".gitignore", []byte(gitignoreContent), 0644); err != nil {
		t.Fatal(err)
	}

	resWithGitignore, err := tool.Execute(ctx, `{"path":".","recursive":true,"max_depth":4}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(resWithGitignore, "node_modules") {
		t.Errorf("node_modules should be ignored by .gitignore, got:\n%s", resWithGitignore)
	}
	if strings.Contains(resWithGitignore, "dist") {
		t.Errorf("dist should be ignored by .gitignore, got:\n%s", resWithGitignore)
	}
	if strings.Contains(resWithGitignore, ".DS_Store") {
		t.Errorf(".DS_Store should be ignored by .gitignore, got:\n%s", resWithGitignore)
	}
	if !strings.Contains(resWithGitignore, "vendor/") {
		t.Errorf("vendor should remain present since not in .gitignore, got:\n%s", resWithGitignore)
	}
	if !strings.Contains(resWithGitignore, "src/main.go") {
		t.Errorf("src/main.go should remain present, got:\n%s", resWithGitignore)
	}
}

func TestListFilesTool_QueryIgnoredDirectly(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	if err := os.MkdirAll(".git", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".git", "config"), []byte("config"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("ignored_dir", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("ignored_dir", "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".gitignore", []byte("ignored_dir/\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &ListFilesTool{}
	ctx := context.Background()

	// Querying .git directly returns "no files found"
	out, err := tool.Execute(ctx, `{"path":".git"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "no files found" {
		t.Errorf("expected 'no files found' for .git, got: %q", out)
	}

	// Querying ignored_dir directly returns "no files found"
	out, err = tool.Execute(ctx, `{"path":"ignored_dir"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "no files found" {
		t.Errorf("expected 'no files found' for ignored_dir, got: %q", out)
	}
}
