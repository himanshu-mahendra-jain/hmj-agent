package terminal

import "testing"

// TestWrapPromptLineWideCharacters checks the prompt layout in terminal columns: wide
// characters take two columns, and one that does not fit in the last column wraps whole.
func TestWrapPromptLineWideCharacters(t *testing.T) {
	cases := []struct {
		name           string
		line           string
		cursor, width  int
		rows, row, col int
	}{
		{"ascii at the end", "abc", 3, 10, 1, 0, 5},
		{"wide at the end", "日本語", 3, 20, 1, 0, 8},
		{"before the last wide rune", "日本語", 2, 20, 1, 0, 6},
		{"mixed", "a日b", 2, 20, 1, 0, 5},
		// "> " + 日本 fills 6 of 7 columns: 語 does not fit in the last one and wraps whole.
		{"wide rune wraps whole", "日本語", 3, 7, 2, 1, 2},
		{"cursor on the wrapped wide rune", "日本語", 2, 7, 2, 1, 0},
		// A full last row keeps the cursor on that row, at column width.
		{"exactly full row", "abcd", 4, 6, 1, 0, 6},
		{"ascii wraps", "abcdefgh", 8, 6, 2, 1, 4},
		{"empty line", "", 0, 10, 1, 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, row, col := wrapPromptLine("> ", []rune(tc.line), tc.cursor, tc.width)
			if rows != tc.rows || row != tc.row || col != tc.col {
				t.Errorf("wrapPromptLine(%q, cursor %d, width %d) = rows %d, cursor %d,%d; want rows %d, cursor %d,%d",
					tc.line, tc.cursor, tc.width, rows, row, col, tc.rows, tc.row, tc.col)
			}
		})
	}
}
