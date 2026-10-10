package logging

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// resetLogger restores the discard logger when the test ends.
func resetLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := Init(""); err != nil {
			t.Errorf("reset logger: %v", err)
		}
	})
}

func TestInitEmptyPathDiscards(t *testing.T) {
	resetLogger(t)

	closeFn, err := Init("")
	if err != nil {
		t.Fatalf("Init(\"\") error: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Fatalf("close error: %v", err)
	}
	if L().Enabled(context.Background(), slog.LevelError) {
		t.Fatal("expected discard logger after Init(\"\")")
	}
	L().Error("ignored")
}

func TestInitWritesJSONLines(t *testing.T) {
	resetLogger(t)
	path := filepath.Join(t.TempDir(), "nested", "dir", "hmj.log")

	closeFn, err := Init(path)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	L().Debug("first", "k", "v")
	L().Info("second", "n", 2)
	if err := closeFn(); err != nil {
		t.Fatalf("close error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), data)
	}
	wantMsgs := []string{"first", "second"}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		if rec["msg"] != wantMsgs[i] {
			t.Errorf("line %d msg = %v, want %q", i, rec["msg"], wantMsgs[i])
		}
	}
}

func TestInitAppendsAndSwitchesFiles(t *testing.T) {
	resetLogger(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "a.log")
	second := filepath.Join(dir, "b.log")

	if _, err := Init(first); err != nil {
		t.Fatalf("Init(first) error: %v", err)
	}
	L().Info("one")
	closeSecond, err := Init(second)
	if err != nil {
		t.Fatalf("Init(second) error: %v", err)
	}
	L().Info("two")
	// The first file was closed by the second Init, so its close func must be a no-op.
	if _, err := Init(first); err != nil {
		t.Fatalf("Init(first again) error: %v", err)
	}
	if err := closeSecond(); err != nil {
		t.Fatalf("stale close func: %v", err)
	}
	L().Info("three")

	for path, want := range map[string]int{first: 2, second: 1} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if got := strings.Count(string(data), "\n"); got != want {
			t.Errorf("%s has %d records, want %d", filepath.Base(path), got, want)
		}
	}
}

func TestConcurrentLogging(t *testing.T) {
	resetLogger(t)
	path := filepath.Join(t.TempDir(), "concurrent.log")

	closeFn, err := Init(path)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	const workers, perWorker = 8, 50
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				L().Info("tick", "worker", w, "i", i)
			}
		})
	}
	wg.Wait()
	if err := closeFn(); err != nil {
		t.Fatalf("close error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != workers*perWorker {
		t.Fatalf("got %d lines, want %d", len(lines), workers*perWorker)
	}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("interleaved or invalid record %q: %v", line, err)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short unchanged", "abc", 10, "abc"},
		{"exact fit unchanged", "abcdefghij", 10, "abcdefghij"},
		{"ascii cut", "abcdefghijklmnop", 10, "abcdefg..."},
		{"budget below ellipsis", "abcdef", 2, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.in, tc.max); got != tc.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestTruncateKeepsUTF8Valid(t *testing.T) {
	in := strings.Repeat("héllo wörld 世界 ", 5)
	for maxBytes := range 60 {
		got := Truncate(in, maxBytes)
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate at %d produced invalid UTF-8: %q", maxBytes, got)
		}
		if len(got) > maxBytes {
			t.Fatalf("Truncate at %d returned %d bytes: %q", maxBytes, len(got), got)
		}
	}
}
