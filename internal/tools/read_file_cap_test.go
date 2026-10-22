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

// writeReadFixture writes content to a fresh file and returns its absolute path.
func writeReadFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readWith runs read_file on path with the given limit (0 for none) and offset (0 for none).
func readWith(t *testing.T, path string, offset, limit int) string {
	t.Helper()
	args, err := json.Marshal(map[string]any{"path": path, "offset": offset, "limit": limit})
	if err != nil {
		t.Fatal(err)
	}
	out, err := (&ReadFileTool{}).Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	return out
}

func numberedLines(n int, width int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "%-*d\n", width, i)
	}
	return sb.String()
}

// TestReadFileDefaultLineCap checks that a read with no limit stops at MaxReadFileLines and that the
// trailer names the offset that continues it, and that the next read picks up exactly there.
func TestReadFileDefaultLineCap(t *testing.T) {
	path := writeReadFixture(t, numberedLines(3000, 4))
	out := readWith(t, path, 0, 0)

	wantTrailer := "--- truncated: showing lines 1-2000 of 3000; call read_file again with offset=2001 ---"
	if !strings.HasSuffix(out, wantTrailer) {
		t.Fatalf("trailer missing, output ends with %q", out[max(0, len(out)-120):])
	}
	if got := strings.Count(out, "\n"); got != 2000 {
		t.Errorf("returned %d newlines, want 2000 lines plus trailer", got)
	}

	rest := readWith(t, path, 2001, 0)
	if strings.Contains(rest, "truncated") {
		t.Errorf("final chunk must not be truncated: %q", rest[max(0, len(rest)-80):])
	}
	if !strings.HasPrefix(rest, "--- ") || !strings.Contains(rest, "(lines 2001-3000 of 3000)") || !strings.HasSuffix(rest, "3000") {
		t.Errorf("continuation is not lines 2001-3000: header %q, tail %q", rest[:min(len(rest), 80)], rest[max(0, len(rest)-20):])
	}
}

// TestReadFileByteCapWithoutLimit checks that long lines are cut by the byte cap before the line cap,
// and that the trailer reports where to continue.
func TestReadFileByteCapWithoutLimit(t *testing.T) {
	line := strings.Repeat("x", 4095)
	path := writeReadFixture(t, strings.Repeat(line+"\n", 100))
	out := readWith(t, path, 0, 0)

	// Each line is 4096 bytes with its newline, so exactly 64 lines fill the 256KB cap.
	want := "--- truncated: showing lines 1-64 of 100; call read_file again with offset=65 ---"
	if !strings.HasSuffix(out, want) {
		t.Fatalf("trailer = %q, want suffix %q", out[max(0, len(out)-120):], want)
	}
	if len(out) > MaxReadFileBytes+len(want)+2 {
		t.Errorf("output is %d bytes, over the %d byte cap", len(out), MaxReadFileBytes)
	}
}

// TestReadFileExplicitLimitHonouredUpToByteCap checks that an explicit limit within the caps returns
// exactly those lines with no trailer, and that a limit past the byte cap is cut with a trailer.
func TestReadFileExplicitLimitHonouredUpToByteCap(t *testing.T) {
	small := writeReadFixture(t, numberedLines(50, 3))
	out := readWith(t, small, 0, 10)
	// The range header and ten lines, no trailer.
	if strings.Contains(out, "truncated") || strings.Count(out, "\n") != 10 {
		t.Errorf("limit 10 within caps: got %q", out)
	}

	big := writeReadFixture(t, strings.Repeat(strings.Repeat("y", 4095)+"\n", 100))
	out = readWith(t, big, 0, 100)
	if !strings.Contains(out, "--- truncated: showing lines 1-64 of 100; call read_file again with offset=65 ---") {
		t.Errorf("explicit limit past the byte cap must be cut with a trailer, got tail %q", out[max(0, len(out)-120):])
	}
}

// TestReadFileCutsOverlongLineAndSaysSo checks that one line over the byte cap is cut and reported,
// rather than returned silently truncated.
func TestReadFileCutsOverlongLineAndSaysSo(t *testing.T) {
	path := writeReadFixture(t, strings.Repeat("é", 200*1024)) // 2 bytes per rune, 400KB, one line
	out := readWith(t, path, 0, 0)
	if !strings.Contains(out, "is longer than 262144 bytes and was cut") {
		t.Fatalf("overlong line not reported, tail %q", out[max(0, len(out)-100):])
	}
	if !strings.HasPrefix(out, "é") {
		t.Errorf("output does not start with the file content")
	}
}

// TestCapLinesToBytesKeepsRuneBoundary checks the cut never splits a multi-byte rune.
func TestCapLinesToBytesKeepsRuneBoundary(t *testing.T) {
	long := strings.Repeat("a", MaxReadFileBytes-1) + "é" // the é straddles the cap
	kept, cut := capLinesToBytes([]string{long}, MaxReadFileBytes)
	if !cut || len(kept) != 1 {
		t.Fatalf("kept %d lines, cut %v; want one cut line", len(kept), cut)
	}
	if strings.ContainsRune(kept[0], '�') || len(kept[0]) > MaxReadFileBytes {
		t.Errorf("cut line is %d bytes and invalid UTF-8 at the boundary", len(kept[0]))
	}
}
