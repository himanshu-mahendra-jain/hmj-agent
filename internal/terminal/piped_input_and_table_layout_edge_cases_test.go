package terminal

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestReadPipedInputMapsShortcutsAndJoinsLines checks the input read when stdin is not a
// terminal: a lone shortcut byte maps to its action, "/" to the cheatsheet, Ctrl+C and Esc
// to nothing, and lines already buffered are joined, the last one even without a newline.
func TestReadPipedInputMapsShortcutsAndJoinsLines(t *testing.T) {
	tests := []struct {
		in         string
		wantText   string
		wantAction ShortcutAction
	}{
		{"first\nsecond", "first\nsecond", ActionNone},
		{"/\n", "/shortcuts", ActionNone},
		{"\x03\n", "", ActionNone},
		{"\x1b\n", "", ActionNone},
		{"\x0e\n", "", ActionNew},
		{"\x12\n", "", ActionResume},
		{"\x0f\n", "", ActionModels},
		{"\x19\n", "", ActionHistory},
		{"  padded  \n", "padded", ActionNone},
	}
	for _, tc := range tests {
		text, action, err := readPipedInput(bufio.NewReader(strings.NewReader(tc.in)))
		if err != nil || text != tc.wantText || action != tc.wantAction {
			t.Errorf("readPipedInput(%q) = (%q, %v, %v), want (%q, %v, nil)", tc.in, text, action, err, tc.wantText, tc.wantAction)
		}
	}
	if _, _, err := readPipedInput(bufio.NewReader(strings.NewReader(""))); err == nil {
		t.Error("empty input did not report its end")
	}
}

// TestVisualLayoutDefaultsToEightyColumns checks that a width of zero or less lays the
// prompt out at 80 columns rather than dividing by nothing.
func TestVisualLayoutDefaultsToEightyColumns(t *testing.T) {
	ed := &PromptEditor{prompt: "> ", indent: "  ", runes: []rune(strings.Repeat("x", 100)), cursor: 100}
	want := ed.VisualLayout(80)
	for _, w := range []int{0, -5} {
		if got := ed.VisualLayout(w); got.TotalVisualRows != want.TotalVisualRows || got.CursorVisualCol != want.CursorVisualCol {
			t.Errorf("VisualLayout(%d) = %+v, want the 80-column layout %+v", w, got, want)
		}
	}
}

// withTerminalSize points os.Stdout at a pipe and os.Stdin at a pseudo terminal of cols
// columns for the rest of the test.
func withTerminalSize(t *testing.T, cols int) {
	t.Helper()
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("PTY not available in environment: %v", err)
	}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: uint16(cols)}); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, w
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		for _, f := range []*os.File{master, slave, r, w} {
			f.Close()
		}
	})
}

// TestTerminalWidthFallsBackToStdin checks that with output redirected the width comes from
// the terminal on stdin, and that tables are capped at 140 columns on a wide terminal.
func TestTerminalWidthFallsBackToStdin(t *testing.T) {
	withTerminalSize(t, 200)
	if w := getTerminalWidth(); w != 200 {
		t.Errorf("getTerminalWidth() = %d, want 200 from stdin", w)
	}
	if w := renderTableWidth(); w != 140 {
		t.Errorf("renderTableWidth() = %d, want the 140-column cap", w)
	}
}

// TestTableLayoutEdgeCases renders tables that need the fallbacks of the layout: more
// columns than fit even at the minimum width, a header that wraps, and separator rows that
// are not quite separators.
func TestTableLayoutEdgeCases(t *testing.T) {
	withTheme(t, false, false)
	withTerminalSize(t, 40)

	t.Run("too many wide columns keep a floor of 8", func(t *testing.T) {
		long := strings.Repeat("abcdefgh ", 4)
		tbl := &Table{Headers: make([]string, 6), Align: make([]Alignment, 6), Rows: [][]string{make([]string, 6)}}
		for i := range 6 {
			tbl.Headers[i], tbl.Rows[0][i] = "h", long
		}
		rows := strings.Split(strings.TrimSuffix(tbl.Render(), "\n"), "\n")
		w := VisibleWidth(rows[0])
		if w < 1+6*(8+3) {
			t.Errorf("table is %d columns wide, want each of 6 columns at least 8 wide", w)
		}
		for _, r := range rows {
			if VisibleWidth(r) != w {
				t.Errorf("row %q is not %d columns wide", r, w)
			}
		}
	})
	t.Run("long header wraps", func(t *testing.T) {
		tbl := &Table{
			Headers: []string{"id", "a header far too long to stay on one line of this table"},
			Align:   []Alignment{AlignLeft, AlignLeft},
			Rows:    [][]string{{"1", strings.Repeat("value ", 8)}},
		}
		out := tbl.Render()
		if !strings.Contains(out, "| id |") || strings.Count(out, "\n") < 6 {
			t.Errorf("header did not wrap onto several lines:\n%s", out)
		}
	})
	t.Run("separator rows", func(t *testing.T) {
		for _, line := range []string{"| : |", "|---|:x:|", "| |", "|"} {
			if isSeparatorRow(line) {
				t.Errorf("isSeparatorRow(%q) = true", line)
			}
		}
		if !isSeparatorRow("| :--- | ---: |") {
			t.Error("a real separator row was rejected")
		}
	})
}

// TestDumbTerminalGetsNoSpinnerAnimation checks that TERM=dumb, which prints escape
// sequences literally, is not treated as an interactive terminal.
func TestDumbTerminalGetsNoSpinnerAnimation(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if isInteractiveStdout() {
		t.Error("TERM=dumb reported as interactive")
	}
}

// TestAccessibleMenuReadsStdinWithoutReader checks that the screen-reader menu reads the
// terminal itself when no reader is passed, and that "r" with a non-number filters instead
// of renaming.
func TestAccessibleMenuReadsStdinWithoutReader(t *testing.T) {
	setAccessible(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	swapStdin(t, r)
	w.WriteString("r x new\n*\n2\n")
	renamed := false
	cb := &SelectCallbacks{OnRename: func(SelectItem, string) (string, error) { renamed = true; return "", nil }}
	var (
		item SelectItem
		ok   bool
	)
	out := captureStdout(t, func() {
		item, ok, err = InteractiveSelect("pick", numberedItems(3), 0, nil, cb)
	})
	if err != nil || !ok || item.ID != "id2" {
		t.Fatalf("got (%v, %v, %v), want item 2 chosen", item, ok, err)
	}
	if renamed || !strings.Contains(out, "filter: r x new") {
		t.Errorf("\"r x new\" should filter, not rename; output %q", out)
	}
}
