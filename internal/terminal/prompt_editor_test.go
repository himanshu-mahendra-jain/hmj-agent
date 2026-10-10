package terminal

import "testing"

func TestPromptEditorLineEditing(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		cursor     int
		op         func(*PromptEditor)
		wantText   string
		wantCursor int
	}{
		// Ctrl+A: start of current line
		{"line start from middle", "abc", 2, (*PromptEditor).MoveLineStart, "abc", 0},
		{"line start on second line", "one\ntwo three", 10, (*PromptEditor).MoveLineStart, "one\ntwo three", 4},
		{"line start already at start", "one\ntwo", 4, (*PromptEditor).MoveLineStart, "one\ntwo", 4},
		{"line start empty", "", 0, (*PromptEditor).MoveLineStart, "", 0},

		// Ctrl+E: end of current line
		{"line end from middle", "abc", 1, (*PromptEditor).MoveLineEnd, "abc", 3},
		{"line end on second line", "one\ntwo three", 5, (*PromptEditor).MoveLineEnd, "one\ntwo three", 13},
		{"line end stops before newline", "one\ntwo", 0, (*PromptEditor).MoveLineEnd, "one\ntwo", 3},

		// Ctrl+U: delete from line start to cursor
		{"delete to line start", "hello world", 5, (*PromptEditor).DeleteToLineStart, " world", 0},
		{"delete to line start on second line", "one\ntwo three", 10, (*PromptEditor).DeleteToLineStart, "one\nree", 4},
		{"delete to line start at start is no-op", "abc", 0, (*PromptEditor).DeleteToLineStart, "abc", 0},

		// Ctrl+K: delete from cursor to line end
		{"delete to line end", "hello world", 5, (*PromptEditor).DeleteToLineEnd, "hello", 5},
		{"delete to line end keeps newline", "one\ntwo", 1, (*PromptEditor).DeleteToLineEnd, "o\ntwo", 1},
		{"delete to line end at end is no-op", "one\ntwo", 3, (*PromptEditor).DeleteToLineEnd, "one\ntwo", 3},

		// Ctrl+W: delete previous word (trailing spaces first)
		{"delete word", "hello world", 11, (*PromptEditor).DeleteWordBackward, "hello ", 6},
		{"delete word skips trailing spaces", "foo bar  ", 9, (*PromptEditor).DeleteWordBackward, "foo ", 4},
		{"delete word only spaces", "hello   ", 8, (*PromptEditor).DeleteWordBackward, "", 0},
		{"delete word treats tab as space", "word\t", 5, (*PromptEditor).DeleteWordBackward, "", 0},
		{"delete word stops at newline", "one\ntwo", 7, (*PromptEditor).DeleteWordBackward, "one\n", 4},
		{"delete word at line start is no-op", "one\ntwo", 4, (*PromptEditor).DeleteWordBackward, "one\ntwo", 4},
		{"delete word empty is no-op", "", 0, (*PromptEditor).DeleteWordBackward, "", 0},

		// Ctrl+Left / Alt+B: start of the previous word, across lines
		{"word left", "one two", 7, (*PromptEditor).MoveWordLeft, "one two", 4},
		{"word left skips spaces", "one two  ", 9, (*PromptEditor).MoveWordLeft, "one two  ", 4},
		{"word left stops at punctuation", "src/main.go", 11, (*PromptEditor).MoveWordLeft, "src/main.go", 9},
		{"word left keeps underscores", "snake_case", 10, (*PromptEditor).MoveWordLeft, "snake_case", 0},
		{"word left crosses a newline", "one\ntwo", 4, (*PromptEditor).MoveWordLeft, "one\ntwo", 0},
		{"word left at start is a no-op", "one", 0, (*PromptEditor).MoveWordLeft, "one", 0},

		// Ctrl+Right / Alt+F: end of the next word, across lines
		{"word right", "one two", 0, (*PromptEditor).MoveWordRight, "one two", 3},
		{"word right skips spaces", "one  two", 3, (*PromptEditor).MoveWordRight, "one  two", 8},
		{"word right crosses a newline", "one\ntwo", 3, (*PromptEditor).MoveWordRight, "one\ntwo", 7},
		{"word right at end is a no-op", "one", 3, (*PromptEditor).MoveWordRight, "one", 3},

		// Alt+Backspace: delete the word before the cursor, stopping at punctuation
		{"delete word before", "cat src/main.go", 15, (*PromptEditor).DeleteWordBefore, "cat src/main.", 13},
		{"delete word before skips punctuation", "a.b..", 5, (*PromptEditor).DeleteWordBefore, "a.", 2},
		{"delete word before stops at newline", "one\ntwo", 7, (*PromptEditor).DeleteWordBefore, "one\n", 4},
		{"delete word before at line start is a no-op", "one\ntwo", 4, (*PromptEditor).DeleteWordBefore, "one\ntwo", 4},

		// Alt+D / Ctrl+Delete: delete the word after the cursor
		{"delete word after", "one two", 0, (*PromptEditor).DeleteWordAfter, " two", 0},
		{"delete word after skips spaces", "one two", 3, (*PromptEditor).DeleteWordAfter, "one", 3},
		{"delete word after stops at newline", "one\ntwo", 3, (*PromptEditor).DeleteWordAfter, "one\ntwo", 3},
		{"delete word after at end is a no-op", "one", 3, (*PromptEditor).DeleteWordAfter, "one", 3},

		// Insert uses slices.Insert at the cursor
		{"insert in middle", "ac", 1, func(e *PromptEditor) { e.Insert('b') }, "abc", 2},
		{"insert at end", "ab", 2, func(e *PromptEditor) { e.Insert('c') }, "abc", 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ed := NewPromptEditor(tc.text)
			ed.cursor = tc.cursor
			ed.historyIndex = 2

			tc.op(ed)

			if got := ed.Text(); got != tc.wantText {
				t.Errorf("text = %q, want %q", got, tc.wantText)
			}
			if ed.Cursor() != tc.wantCursor {
				t.Errorf("cursor = %d, want %d", ed.Cursor(), tc.wantCursor)
			}
			// Editing resets history navigation; a no-op or pure movement keeps it.
			wantHistory := 2
			if tc.wantText != tc.text {
				wantHistory = 0
			}
			if ed.historyIndex != wantHistory {
				t.Errorf("historyIndex = %d, want %d", ed.historyIndex, wantHistory)
			}
		})
	}
}
