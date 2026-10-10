package e2e

import (
	"slices"
	"testing"
)

// TestScreenEmulator checks the test terminal emulator itself, so screen assertions in the
// other tests fail only for real rendering problems.
func TestScreenEmulator(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		out        string
		rows, cols int
		want       []string // visible rows, trailing blanks trimmed
		row, col   int      // cursor
	}{
		{"plain text and CRLF", "ab\r\ncd", 3, 10, []string{"ab", "cd", ""}, 1, 2},
		{"carriage return overwrites", "abc\rX", 2, 10, []string{"Xbc", ""}, 0, 1},
		{"wrap at the margin", "abcdef", 3, 4, []string{"abcd", "ef", ""}, 1, 2},
		{"last column defers the wrap", "abcd", 2, 4, []string{"abcd", ""}, 0, 3},
		{"deferred wrap then CR stays on the row", "abcd\rX", 2, 4, []string{"Xbcd", ""}, 0, 1},
		{"scroll at the bottom", "1\r\n2\r\n3", 2, 4, []string{"2", "3"}, 1, 1},
		{"cursor up and erase below", "a\r\nb\r\nc\x1b[2A\r\x1b[J", 3, 4, []string{"", "", ""}, 0, 0},
		{"erase to end of line", "abcdef\x1b[3D\x1b[K", 1, 10, []string{"abc"}, 0, 3},
		{"erase whole line keeps the cursor", "abc\x1b[2K", 1, 10, []string{""}, 0, 3},
		{"cursor position", "\x1b[2;3HX", 3, 5, []string{"", "  X", ""}, 1, 3},
		{"forward and back", "ab\x1b[3CX\x1b[5DY", 1, 10, []string{"aY   X"}, 0, 2},
		{"colors take no space", "\x1b[1;38;5;210mred\x1b[0m!", 1, 10, []string{"red!"}, 0, 4},
		{"OSC is not drawn", "\x1b]133;A\x07> \x1b]133;B\x07hi", 1, 10, []string{"> hi"}, 0, 4},
		{"wide runes take two cells", "日本x", 1, 10, []string{"日本x"}, 0, 5},
		{"wide rune wraps whole", "abc日", 2, 4, []string{"abc", "日"}, 1, 2},
		{"tab stops", "a\tb", 1, 20, []string{"a       b"}, 0, 9},
		{"save and restore cursor", "ab\x1b7cd\x1b8X", 1, 10, []string{"abXd"}, 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := renderScreen(tc.out, tc.rows, tc.cols, nil)
			if got := v.Lines(); !slices.Equal(got, tc.want) {
				t.Errorf("rows = %q, want %q\n%s", got, tc.want, v.Dump())
			}
			if r, c := v.Cursor(); r != tc.row || c != tc.col {
				t.Errorf("cursor = %d,%d, want %d,%d\n%s", r, c, tc.row, tc.col, v.Dump())
			}
		})
	}

	t.Run("erase scrollback and OSC log", func(t *testing.T) {
		v := renderScreen("1\r\n2\r\n3\x1b]133;C\x07\x1b[H\x1b[2J\x1b[3J", 2, 4, nil)
		if !v.clearedScroll || len(v.scrollback) != 0 || v.Text() != "" {
			t.Errorf("screen and scrollback not cleared: cleared=%v scrollback=%q text=%q", v.clearedScroll, v.scrollback, v.Text())
		}
		if !slices.Equal(v.oscs, []string{"133;C"}) {
			t.Errorf("oscs = %q, want [133;C]", v.oscs)
		}
	})
	t.Run("resize at an offset", func(t *testing.T) {
		out := "abcdef" + "ghij"
		v := renderScreen(out, 2, 10, []screenResize{{offset: 6, rows: 2, cols: 4}})
		// "abcdef" fits one 10-column row. Shrinking to 4 columns cuts the row to "abcd" and
		// clamps the cursor (col 6) to the last column, so "g" overwrites "d" and "hij" wraps.
		if got := v.Lines(); !slices.Equal(got, []string{"abcg", "hij"}) {
			t.Errorf("rows = %q\n%s", got, v.Dump())
		}
	})
}
