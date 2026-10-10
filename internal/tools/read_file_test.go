package tools

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// readLineRangeReference is the straightforward line splitter readLineRange must agree with.
func readLineRangeReference(content string, offset, limit int) ([]string, int) {
	var selected []string
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		if i+1 >= offset && (limit <= 0 || len(selected) < limit) {
			selected = append(selected, strings.TrimRight(l, "\r\n"))
		}
	}
	return selected, len(lines)
}

// Lines longer than the 4096-byte read buffer, including one ending exactly on it at
// the end of the file, are counted once and returned whole.
func TestReadLineRangeMatchesReference(t *testing.T) {
	const bufSize = 4096
	pieces := []string{"", "a", "short line", "crlf\r", strings.Repeat("x", bufSize-1), strings.Repeat("y", bufSize),
		strings.Repeat("z", bufSize+1), strings.Repeat("w", 3*bufSize+7)}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		var sb strings.Builder
		for n := rng.IntN(8); n >= 0; n-- {
			sb.WriteString(pieces[rng.IntN(len(pieces))])
			if n > 0 || rng.IntN(2) == 0 {
				sb.WriteByte('\n')
			}
		}
		content := sb.String()
		if content == "" {
			continue
		}
		offset, limit := 1+rng.IntN(6), rng.IntN(4)
		got, gotTotal := readLineRange(strings.NewReader(content), offset, limit)
		want, wantTotal := readLineRangeReference(content, offset, limit)
		if gotTotal != wantTotal || !slices.Equal(got, want) {
			t.Fatalf("readLineRange(len %d, offset %d, limit %d) = %d lines (total %d), want %d lines (total %d)",
				len(content), offset, limit, len(got), gotTotal, len(want), wantTotal)
		}
	}
}
