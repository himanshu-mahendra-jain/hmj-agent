package e2e

import (
	"strings"
	"testing"
)

// promptRow returns the row holding the prompt glyph followed by text, or -1.
func promptRow(v *vtScreen, text string) int { return v.RowOf("> " + text) }

// TestPromptCursorAfterTab types a Tab between two letters. Whatever the Tab looks like on
// screen, the cursor must sit right after the last character typed, not inside the gap.
func TestPromptCursorAfterTab(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("a\tb")
	v := s.ExpectScreen("both letters on the prompt row", func(v *vtScreen) bool {
		return v.RowOf("> a") >= 0 && strings.Contains(v.lineText(v.RowOf("> a")), "b")
	})
	row := v.RowOf("> a")
	line := []rune(v.lineText(row))
	bCol := -1
	for i, r := range line {
		if r == 'b' {
			bCol = i
		}
	}
	if r, c := v.Cursor(); r != row || c != bCol+1 {
		t.Errorf("cursor at %d,%d, want right after b at %d,%d\n%s", r, c, row, bCol+1, v.Dump())
	}
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `a\tb`) {
		t.Errorf("request does not carry a<Tab>b: %s", req)
	}
	s.Quit()
}

// TestPromptWideCharactersWrapWhole types CJK characters across the right margin of a
// narrow terminal: a double-width character that does not fit in the last column moves to
// the next row whole, and the cursor follows it there. Backspace then steps back over the
// wrap.
func TestPromptWideCharactersWrapWhole(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 21 // "> " plus 9 wide characters is 20 columns: one column is left over
	e.WriteConfig(p.config("model-a"))
	s := e.Start()
	s.ExpectPrompt()
	s.Type("一二三四五六七八九十")
	v := s.ExpectScreen("the tenth character wrapped to the next row", func(v *vtScreen) bool {
		first := promptRow(v, "一二三四五六七八九")
		return first >= 0 && v.RowOf("十") == first+1
	})
	first := promptRow(v, "一二三四五六七八九")
	if r, c := v.Cursor(); r != first+1 || c != 2 {
		t.Errorf("cursor at %d,%d, want after 十 at %d,2\n%s", r, c, first+1, v.Dump())
	}
	s.Type("\x7f") // Backspace removes 十: the cursor goes back to the end of the first row
	s.ExpectScreen("the cursor back on the first row after 九", func(v *vtScreen) bool {
		r, c := v.Cursor()
		return v.RowOf("十") < 0 && r == first && c == 20
	})
	s.Type("x\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, "一二三四五六七八九x") {
		t.Errorf("request does not carry the edited prompt: %s", req)
	}
	s.Quit()
}

// TestPromptEmojiAndCombiningMarks types an emoji outside the basic pictograph block, a
// letter with a combining accent and a ZWJ sequence, and checks the cursor after each: the
// emoji takes two columns, the accent and the joiner none.
func TestPromptEmojiAndCombiningMarks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, text string
		cols       int // columns the text takes after "> "
	}{
		{"rocket emoji", "🚀x", 3},
		{"combining acute accent", "café!", 5},
		{"zero-width joiner", "a\u200db", 2},
		{"variation selector", "❤️z", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newProvider(t)
			_, s := start(t, p)
			s.Type(tc.text)
			s.ExpectScreen("the cursor right after the text", func(v *vtScreen) bool {
				row, col := v.Cursor()
				return v.RowOf("> ") == row && col == 2+tc.cols
			})
			s.Type("\x1b[D\x1b[D") // two Lefts, then type in the middle
			s.Type("_\r")
			s.Expect("ok")
			s.ExpectPrompt()
			rs := []rune(tc.text)
			want := string(rs[:len(rs)-2]) + "_" + string(rs[len(rs)-2:])
			if req := p.LastRequest(); !strings.Contains(req, want) {
				t.Errorf("request does not carry %q: %s", want, req)
			}
			s.Quit()
		})
	}
}

// TestLongLineEditedInTheMiddle types a single line three rows long, moves into the middle
// row with the arrow keys and edits there. Every row is redrawn in place: no stale copy of
// the text is left above the prompt.
func TestLongLineEditedInTheMiddle(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 30
	e.WriteConfig(p.config("model-a"))
	s := e.Start()
	s.ExpectPrompt()
	text := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 3) // 78 runes: rows of 28, 30 and 20 columns
	s.Type(text)
	v := s.ExpectScreen("the prompt over three rows", func(v *vtScreen) bool {
		first := promptRow(v, text[:28])
		return first >= 0 && v.RowOf(text[28:58]) == first+1 && v.RowOf(text[58:]) == first+2
	})
	first := promptRow(v, text[:28])
	// 30 Lefts put the cursor 30 runes from the end: rune 48, column 20 of the middle row.
	s.Type(strings.Repeat("\x1b[D", 30))
	s.ExpectScreen("the cursor in the middle row", func(v *vtScreen) bool {
		r, c := v.Cursor()
		return r == first+1 && c == 20
	})
	s.Type("XYZ")
	edited := text[:48] + "XYZ" + text[48:]
	v = s.ExpectScreen("the edit rewrapped over the rows below", func(v *vtScreen) bool {
		r, c := v.Cursor()
		return v.RowOf(edited[28:58]) == first+1 && v.RowOf(edited[58:]) == first+2 && r == first+1 && c == 23
	})
	if strings.Count(v.Text(), text[:28]) != 1 {
		t.Errorf("the first row is on screen more than once:\n%s", v.Dump())
	}
	s.Type("\x7f\x7f\x7f") // Backspace three times restores the text
	s.ExpectScreen("the original text back", func(v *vtScreen) bool {
		return v.RowOf(text[28:58]) == first+1 && v.RowOf(text[58:]) == first+2
	})
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, text) {
		t.Errorf("request does not carry the text: %s", req)
	}
	s.Quit()
}

// TestBracketedPasteIsSanitized pastes text holding colour codes, a screen-clearing
// sequence, a title-setting OSC string, Backspace and BEL bytes, a Tab and newlines. The
// paste is text: none of it may run as a key or reach the terminal as a control sequence,
// and the prompt stays one prompt until Enter is pressed.
func TestBracketedPasteIsSanitized(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	paste := "\x1b[31mred\x1b[0m\x1b[2J\x1b]0;evil title\x07 one\x08\x07\ttwo\r\nthree\x1b[A\x1b[D\x03\x04end"
	s.Type("\x1b[200~" + paste + "\x1b[201~")
	v := s.ExpectScreen("the pasted lines on the prompt", func(v *vtScreen) bool {
		return v.RowOf("> red") >= 0 && v.RowOf("  three") == v.RowOf("> red")+1
	})
	if v.RowOf("HMJ Agent") < 0 {
		t.Errorf("the paste cleared the screen:\n%s", v.Dump())
	}
	for _, osc := range v.oscs {
		if strings.Contains(osc, "evil") {
			t.Errorf("the paste set the terminal title: %q", osc)
		}
	}
	row := v.RowOf("> red")
	line := v.lineText(row)
	if !strings.Contains(line, "one") || !strings.Contains(line, "two") {
		t.Errorf("pasted words missing from the prompt row %q\n%s", line, v.Dump())
	}
	last := v.lineText(row + 1)
	if !strings.HasSuffix(last, "threeend") {
		t.Errorf("second pasted line = %q, want it to end with threeend (arrow keys and Ctrl+C/D in a paste are text, not keys)", last)
	}
	if r, c := v.Cursor(); r != row+1 || c != len(last) {
		t.Errorf("cursor at %d,%d, want after the paste at %d,%d\n%s", r, c, row+1, len(last), v.Dump())
	}
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	req := p.LastRequest()
	if !strings.Contains(req, `red one\ttwo\nthreeend`) {
		t.Errorf("request does not carry the cleaned paste: %s", req)
	}
	if strings.Contains(req, `\u001b`) || strings.Contains(req, `\u0007`) || strings.Contains(req, `\b`) {
		t.Errorf("a control character from the paste reached the request: %s", req)
	}
	s.Quit()
}

// TestBracketedPasteKeepsSurroundingWhitespace pastes text that starts with a space after
// a typed word, then indented lines: the paste is inserted as is, so the space and the
// indentation survive.
func TestBracketedPasteKeepsSurroundingWhitespace(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("hello")
	s.ExpectScreen("the typed word", func(v *vtScreen) bool { return v.RowOf("> hello") >= 0 })
	s.Type("\x1b[200~ world\x1b[201~")
	s.ExpectScreen("the pasted word after a space", func(v *vtScreen) bool { return v.RowOf("> hello world") >= 0 })
	s.Type("\x1b[200~\n    indented()\n\x1b[201~")
	s.ExpectScreen("the indented line", func(v *vtScreen) bool { return v.RowOf("      indented()") >= 0 })
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `hello world\n    indented()`) {
		t.Errorf("request does not carry the paste as pasted: %s", req)
	}
	s.Quit()
}

// TestBracketedPasteWithWindowsLineEndings pastes CRLF-terminated lines: each line break is
// one newline, not a newline and an empty line.
func TestBracketedPasteWithWindowsLineEndings(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("\x1b[200~alpha\r\nbeta\r\ngamma\x1b[201~")
	s.ExpectScreen("three consecutive rows", func(v *vtScreen) bool {
		first := v.RowOf("> alpha")
		return first >= 0 && v.RowOf("  beta") == first+1 && v.RowOf("  gamma") == first+2
	})
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `alpha\nbeta\ngamma`) {
		t.Errorf("request does not carry one newline per line break: %s", req)
	}
	s.Quit()
}

// TestCtrlSDoesNotFreezeThePrompt presses Ctrl+S (XOFF in a terminal's default settings)
// between two letters, at the prompt and in a menu. It is an unbound key: the screen keeps
// updating without a Ctrl+Q, and nothing is typed for it.
func TestCtrlSDoesNotFreezeThePrompt(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("a\x13b")
	s.ExpectScreen("both letters drawn", func(v *vtScreen) bool { return v.RowOf("> ab") >= 0 })
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `"ab"`) {
		t.Errorf("request does not carry exactly ab: %s", req)
	}
	s.Type("\x0f") // Ctrl+O: the model menu
	s.Expect("select model")
	s.Type("\x13\x1b[B")
	s.ExpectScreen("the highlight moved after Ctrl+S", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "model-b")
	})
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}

// TestCutShortCharacterDoesNotSwallowTheNextKey types a byte that starts a multi-byte
// UTF-8 character but is followed by Enter, as a Latin-1 terminal sends "é". The broken
// character is dropped and Enter still submits the prompt.
func TestCutShortCharacterDoesNotSwallowTheNextKey(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("caf\xe9\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `"caf"`) {
		t.Errorf("request does not carry the text before the broken character: %s", req)
	}
	s.Type("x\xe6\x97y\r") // a three-byte character cut after two bytes, then a letter
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `"xy"`) {
		t.Errorf("the letter after a cut-short character was lost: %s", req)
	}
	s.Quit()
}

// TestClearingAWrappedPromptErasesEveryRow types a prompt that wraps over three rows, moves
// the cursor into the middle row and clears it with Ctrl+C, then in a second run with Esc:
// every row of the old text must go, not just the one the cursor is on.
func TestClearingAWrappedPromptErasesEveryRow(t *testing.T) {
	t.Parallel()
	for _, key := range []struct{ name, keys string }{{"Ctrl+C", "\x03"}, {"Esc", "\x1b"}} {
		t.Run(key.name, func(t *testing.T) {
			t.Parallel()
			p := newProvider(t)
			e := newEnv(t)
			e.Cols = 30
			e.WriteConfig(p.config("model-a"))
			s := e.Start()
			s.ExpectPrompt()
			text := strings.Repeat("abcdefghij", 7)
			s.Type(text)
			s.ExpectScreen("the prompt over three rows", func(v *vtScreen) bool { return v.RowOf(text[58:]) >= 0 })
			s.Type(strings.Repeat("\x1b[D", 30))
			s.ExpectScreen("the cursor in the middle row", func(v *vtScreen) bool {
				row, _ := v.Cursor()
				return v.RowOf(text[58:]) == row+1
			})
			s.Type(key.keys)
			v := s.ExpectScreen("an empty prompt", func(v *vtScreen) bool {
				row, col := v.Cursor()
				return v.RowOf("abcdefghij") < 0 && v.lineText(row) == ">" && col == 2
			})
			// At 30 columns the model line wraps; its last row ends in "default]".
			if row, _ := v.Cursor(); v.RowOf("default]") != row-1 {
				t.Errorf("the empty prompt is not right below the model line:\n%s", v.Dump())
			}
			// The redrawn prompt keeps its OSC 133 marks, like every other prompt drawn.
			raw := s.Raw()
			if after := raw[strings.LastIndex(raw, "\x1b[J")+3:]; !strings.HasPrefix(after, "\x1b]133;A\x07> \x1b]133;B\x07") {
				t.Errorf("the cleared prompt was redrawn without its marks: %q", after)
			}
			s.Type("short\r")
			s.Expect("ok")
			s.ExpectPrompt()
			if req := p.LastRequest(); strings.Contains(req, "abcdefghij") {
				t.Errorf("the cleared text was sent: %s", req)
			}
			s.Quit()
		})
	}
}
