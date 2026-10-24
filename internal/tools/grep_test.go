package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFiles creates each file with its content, and its parent directories, under the
// current directory.
func writeFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// grep runs the grep tool with args and returns its result.
// grep runs the tool and returns its output after the header line, which TestGrepHeader checks.
func grep(t *testing.T, args map[string]any) string {
	t.Helper()
	out, err := (&GrepTool{}).Execute(context.Background(), mustJSON(t, args))
	if err != nil {
		t.Fatalf("grep %v: %v", args, err)
	}
	header, rest, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(header, "[searched for ") {
		t.Fatalf("grep %v: no header line: %q", args, out)
	}
	return rest
}

// The first line states the search as the arguments asked for it, so a model that sent other
// arguments than it meant can see so.
func TestGrepHeader(t *testing.T) {
	grepTree(t)
	tests := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"pattern": "x"}, `[searched for "x" in . (no path given)]`},
		{map[string]any{"pattern": `a\.b`, "path": "src", "glob": "*.kt", "case_insensitive": true, "literal": true}, `[searched for "a\\.b" in src, glob "*.kt", case-insensitive, literal]`},
	}
	for _, tc := range tests {
		out, err := (&GrepTool{}).Execute(context.Background(), mustJSON(t, tc.args))
		if header, _, _ := strings.Cut(out, "\n"); err != nil || header != tc.want {
			t.Errorf("grep %v: header %q, %v; want %q", tc.args, header, err, tc.want)
		}
	}
}

// grepTree makes the tree most grep tests search, in a fresh working directory.
func grepTree(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	writeFiles(t, map[string]string{
		".gitignore":              "build/\n*.log\n",
		"main.go":                 "package main\n\nfunc main() {\n\tTODO()\n}\n",
		"src/app/Handler.kt":      "class Handler {\n  // todo: retry\n}\n",
		"src/test/HandlerTest.kt": "class HandlerTest {\n  fun testTODO() {}\n}\n",
		"web/view.tsx":            "export const TODO = 1\n",
		"web/view.ts":             "// TODO later\n",
		"build/out.go":            "TODO in build output\n",
		"debug.log":               "TODO in a log\n",
		".git/HEAD":               "TODO in git\n",
		"image.bin":               "TODO\x00binary",
	})
}

// TestGrepFindsMatchingFiles checks the default mode: the files with a match, walked in order,
// leaving out .git, ignored paths and binary files.
func TestGrepFindsMatchingFiles(t *testing.T) {
	grepTree(t)
	got := grep(t, map[string]any{"pattern": "TODO"})
	if want := "main.go\nsrc/test/HandlerTest.kt\nweb/view.ts\nweb/view.tsx"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestGrepOptions checks case_insensitive, literal, glob, a path and the count mode.
func TestGrepOptions(t *testing.T) {
	grepTree(t)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"case insensitive", map[string]any{"pattern": "todo:", "case_insensitive": true}, "src/app/Handler.kt"},
		{"literal", map[string]any{"pattern": "testTODO()", "literal": true}, "src/test/HandlerTest.kt"},
		{"regular expression", map[string]any{"pattern": `^export const \w+ =`}, "web/view.tsx"},
		{"name glob", map[string]any{"pattern": "TODO", "glob": "*.go"}, "main.go"},
		{"brace glob", map[string]any{"pattern": "TODO", "glob": "*.{ts,kt}"}, "src/test/HandlerTest.kt\nweb/view.ts"},
		{"path glob", map[string]any{"pattern": "class", "glob": "src/**/*Test.kt"}, "src/test/HandlerTest.kt"},
		{"path glob relative to path", map[string]any{"pattern": "class", "path": "src", "glob": "test/*.kt"}, "src/test/HandlerTest.kt"},
		{"a directory", map[string]any{"pattern": "class", "path": "src/app"}, "src/app/Handler.kt"},
		{"one file", map[string]any{"pattern": "TODO", "path": "main.go", "output_mode": "count"}, "main.go:1"},
		{"an ignored file named", map[string]any{"pattern": "TODO", "path": "debug.log"}, "debug.log"},
		{"count", map[string]any{"pattern": "class|fun ", "output_mode": "count"}, "src/app/Handler.kt:1\nsrc/test/HandlerTest.kt:2"},
		{"no match", map[string]any{"pattern": "absent"}, "no matches found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := grep(t, tc.args); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestGrepContent checks the content mode: path:line:text for matches, path-line-text for
// context, and "--" between groups that do not touch.
func TestGrepContent(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFiles(t, map[string]string{
		"a.txt": "one\nhit 2\nthree\nfour\nfive\nsix\nhit 7\nhit 8\nnine\r\n",
		"b.txt": "hit\n",
	})
	got := grep(t, map[string]any{"pattern": "hit", "output_mode": "content"})
	if want := "a.txt:2:hit 2\na.txt:7:hit 7\na.txt:8:hit 8\nb.txt:1:hit"; got != want {
		t.Errorf("without context got:\n%s\nwant:\n%s", got, want)
	}
	got = grep(t, map[string]any{"pattern": "hit", "output_mode": "content", "context": 1})
	want := strings.Join([]string{
		"a.txt-1-one", "a.txt:2:hit 2", "a.txt-3-three",
		"--",
		"a.txt-6-six", "a.txt:7:hit 7", "a.txt:8:hit 8", "a.txt-9-nine",
		"--",
		"b.txt:1:hit",
	}, "\n")
	if got != want {
		t.Errorf("with context got:\n%s\nwant:\n%s", got, want)
	}
}

// TestGrepLimits checks head_limit, the note that more matches exist, and the cut of a long line.
func TestGrepLimits(t *testing.T) {
	t.Chdir(t.TempDir())
	var many strings.Builder
	for range 10 {
		many.WriteString("match\n")
	}
	writeFiles(t, map[string]string{
		"many.txt": many.String(),
		"long.txt": "match " + strings.Repeat("x", 2000) + "\n",
	})
	got := grep(t, map[string]any{"pattern": "match", "output_mode": "content", "path": "many.txt", "head_limit": 3})
	if want := "many.txt:1:match\nmany.txt:2:match\nmany.txt:3:match\n[stopped after 3 lines; there are more matches: narrow the pattern, path or glob, or raise head_limit]"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	got = grep(t, map[string]any{"pattern": "match", "output_mode": "content", "path": "long.txt"})
	if len(got) > grepMaxLineBytes+50 || !strings.HasSuffix(got, " … [1506 more bytes]") {
		t.Errorf("long line not cut: %d bytes ending %q", len(got), got[max(len(got)-30, 0):])
	}
}

// TestGrepOutsideTheWorkingDirectory checks that files outside the working directory are shown
// by absolute path, which read_file accepts as it is.
func TestGrepOutsideTheWorkingDirectory(t *testing.T) {
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "f.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if got, want := grep(t, map[string]any{"pattern": "needle", "path": other}), filepath.Join(other, "f.txt"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestGrepErrors checks the errors for a bad pattern, glob, mode or path.
func TestGrepErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"pattern": "func("}, "invalid pattern"},
		{map[string]any{"pattern": "x", "glob": "[a"}, "invalid glob"},
		{map[string]any{"pattern": "x", "output_mode": "lines"}, "invalid output_mode"},
		{map[string]any{"pattern": "x", "path": "missing"}, "path not found"},
		{map[string]any{"pattern": ""}, "pattern is required"},
	} {
		_, err := (&GrepTool{}).Execute(context.Background(), mustJSON(t, tc.args))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want one containing %q", tc.args, err, tc.want)
		}
	}
}

// TestGrepStopsWhenCancelled checks that a cancelled search stops with the context's error.
func TestGrepStopsWhenCancelled(t *testing.T) {
	grepTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&GrepTool{}).Execute(ctx, mustJSON(t, map[string]any{"pattern": "TODO"})); err == nil {
		t.Error("a cancelled search returned no error")
	}
}
