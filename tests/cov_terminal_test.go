package tests

import (
	"strings"
	"testing"

	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
)

// TestTableStreamerAccessibleOutputIsASCII streams a markdown table in chunks with accessibility
// on and checks the grid is drawn with ASCII only and every line is the same width.
func TestTableStreamerAccessibleOutputIsASCII(t *testing.T) {
	theme.SetAccessibility(true)
	theme.SetEnabled(false)
	t.Cleanup(func() { theme.SetAccessibility(false) })

	var out strings.Builder
	s := terminal.NewTableStreamer(func(x string) { out.WriteString(x) })
	for _, chunk := range []string{"Here:\n| Tool | Use |\n", "|:--|--:|\n| read_file | 日本語 |\n", "done\n"} {
		s.Write(chunk)
	}
	s.Flush()
	got := out.String()
	if !strings.HasPrefix(got, "Here:\n") || !strings.HasSuffix(got, "done\n") {
		t.Errorf("prose around table lost:\n%s", got)
	}
	if strings.ContainsAny(got, "┌┐└┘│─┼") {
		t.Errorf("box-drawing glyphs in accessible output:\n%s", got)
	}
	grid := strings.Split(strings.TrimRight(strings.SplitAfter(got, "Here:\n")[1], "\n"), "\n")
	grid = grid[:len(grid)-1] // drop "done"
	width := terminal.VisibleWidth(grid[0])
	for _, line := range grid {
		if terminal.VisibleWidth(line) != width {
			t.Errorf("grid line %q is %d wide, want %d", line, terminal.VisibleWidth(line), width)
		}
	}
}
