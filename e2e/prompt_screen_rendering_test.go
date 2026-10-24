package e2e

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestNewConversationClearsScreen checks that Ctrl+N clears the screen and the scrollback,
// redraws the banner, and keeps the earlier conversation in /resume.
func TestNewConversationClearsScreen(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(say("first answer"))
	s.Type("first chat\r")
	s.Expect("first answer")
	s.ExpectPrompt()
	s.Type("\x0e") // Ctrl+N
	s.Expect("created conversation")
	s.ExpectPrompt()
	v := s.ExpectScreen("the banner on a cleared screen", func(v *vtScreen) bool {
		return v.RowOf("HMJ Agent") >= 0 && v.RowOf("created conversation") >= 0
	})
	if v.RowOf("first answer") >= 0 || v.RowOf("first chat") >= 0 {
		t.Errorf("the previous conversation is still on screen:\n%s", v.Dump())
	}
	if !v.clearedScroll || strings.Contains(strings.Join(v.scrollback, "\n"), "first answer") {
		t.Errorf("scrollback not cleared (ED 3 received: %v): %q", v.clearedScroll, v.scrollback)
	}
	s.Type("\x12")
	s.Expect("2/2 matches")
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}

// TestLongPromptWrapsAndCursorFollows types a prompt longer than the terminal is wide and
// checks the wrapped rows and the cursor while moving to the start and inserting there.
func TestLongPromptWrapsAndCursorFollows(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 40
	e.WriteConfig(p.config("model-a"))
	s := e.Start()
	s.ExpectPrompt()

	text := strings.Repeat("abcdefghij", 7) // 70 runes: "> " plus 38 on the first row, 32 on the next
	s.Type(text)
	v := s.ExpectScreen("the prompt wrapped over two rows", func(v *vtScreen) bool {
		return v.RowOf("> "+text[:38]) >= 0 && v.RowOf(text[38:]) >= 0
	})
	first := v.RowOf("> " + text[:38])
	if row, col := v.Cursor(); row != first+1 || col != 32 {
		t.Errorf("cursor at %d,%d, want the end of the text at %d,32\n%s", row, col, first+1, v.Dump())
	}

	s.Type("\x01") // Ctrl+A: start of the line
	s.ExpectScreen("the cursor after the prompt glyph", func(v *vtScreen) bool {
		row, col := v.Cursor()
		return row == first && col == 2
	})
	s.Type("Z")
	s.ExpectScreen("Z inserted before the text, the rest rewrapped", func(v *vtScreen) bool {
		row, col := v.Cursor()
		return v.RowOf("> Z"+text[:37]) == first && v.RowOf(text[37:]) == first+1 && row == first && col == 3
	})
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, "Z"+text) {
		t.Errorf("request does not carry the edited prompt: %s", req)
	}
	s.Quit()
}

// TestMultiLinePromptEditing pastes two lines, moves up into the first one and edits it.
func TestMultiLinePromptEditing(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("\x1b[200~line one\nline two\x1b[201~")
	v := s.ExpectScreen("both lines of the paste", func(v *vtScreen) bool {
		return v.RowOf("> line one") >= 0 && v.RowOf("  line two") == v.RowOf("> line one")+1
	})
	first := v.RowOf("> line one")
	if row, col := v.Cursor(); row != first+1 || col != 10 {
		t.Errorf("cursor at %d,%d, want the end of line two at %d,10\n%s", row, col, first+1, v.Dump())
	}
	s.Type("\x1b[A") // Up: same column on the line above
	s.ExpectScreen("the cursor at the end of line one", func(v *vtScreen) bool {
		row, col := v.Cursor()
		return row == first && col == 10
	})
	s.Type("!")
	s.ExpectScreen("line one edited, line two unchanged", func(v *vtScreen) bool {
		return v.RowOf("> line one!") == first && v.RowOf("  line two") == first+1
	})
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `line one!\nline two`) {
		t.Errorf("request does not carry the edited two-line prompt: %s", req)
	}
	s.Quit()
}

// TestWidePromptCharactersKeepCursorInPlace types double-width characters and checks that
// the cursor sits right after them and that moving left steps one character at a time.
func TestWidePromptCharactersKeepCursorInPlace(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("日本語")
	v := s.ExpectScreen("the wide characters on the prompt row", func(v *vtScreen) bool {
		return v.RowOf("> 日本語") >= 0
	})
	row := v.RowOf("> 日本語")
	if r, c := v.Cursor(); r != row || c != 8 {
		t.Errorf("cursor at %d,%d, want %d,8 (2 for \"> \" and 2 columns per character)\n%s", r, c, row, v.Dump())
	}
	s.Type("\x1b[D") // Left: before 語
	s.ExpectScreen("the cursor before the last character", func(v *vtScreen) bool {
		r, c := v.Cursor()
		return r == row && c == 6
	})
	s.Type("x\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, "日本x語") {
		t.Errorf("request does not carry the edited prompt: %s", req)
	}
	s.Quit()
}

// TestUnboundControlKeysAreIgnored checks that control keys without a binding type nothing,
// while Tab is still inserted.
func TestUnboundControlKeysAreIgnored(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("\x02a\x0cb\x14c\x07\td\r") // Ctrl+B, Ctrl+L, Ctrl+T, Ctrl+G between the letters
	s.Expect("ok")
	s.ExpectPrompt()
	req := p.LastRequest()
	if !strings.Contains(req, `abc\td`) {
		t.Errorf("request does not carry exactly abc<Tab>d: %s", req)
	}
	if strings.Contains(req, `\u000`) {
		t.Errorf("a control character reached the request: %s", req)
	}
	s.Quit()
}

// TestSlashMenuWithNoMatch filters the command menu down to nothing, then leaves it with Esc;
// the filter text must not end up in the next prompt.
func TestSlashMenuWithNoMatch(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("/")
	s.Expect("commands")
	s.TypeSlowly("zzz")
	s.ExpectScreen("an empty result", func(v *vtScreen) bool {
		return v.RowOf("(no matching results)") >= 0 && v.RowOf("0 matches") >= 0
	})
	s.PressEsc()
	s.Type("hi\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); strings.Contains(req, "zzz") {
		t.Errorf("menu filter leaked into the prompt: %s", req)
	}
	s.Quit()
}

// TestSmallTerminalStillUsable runs hmj in a 12x40 terminal: the menus and the prompt must
// still show their key lines.
func TestSmallTerminalStillUsable(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Rows, e.Cols = 14, 40
	e.WriteConfig(p.config("model-a", "model-b"))
	s := e.Start()
	s.ExpectPrompt()
	s.Type("\x0f") // Ctrl+O
	s.Expect("select model")
	s.ExpectScreen("the model menu with its footer", func(v *vtScreen) bool {
		return v.RowOf("select model") >= 0 && v.RowOf("model-b") >= 0 && v.RowOf("matches") >= 0
	})
	s.Type("\x1b[B\r")
	s.Expect("model-b")
	s.ExpectPrompt()
	s.Type("hi\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.ExpectScreen("the prompt on the last rows", func(v *vtScreen) bool {
		return v.RowOf("[model: mock/model-b · effort: default]") >= v.rows-3
	})
	s.Quit()
}

// TestResizeWhileTyping narrows the terminal in the middle of a prompt. The next key redraws
// the prompt for the new width, and the whole text is sent.
func TestResizeWhileTyping(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 80
	e.WriteConfig(p.config("model-a"))
	s := e.Start()
	s.ExpectPrompt()
	text := strings.Repeat("0123456789", 6)
	s.Type(text)
	s.ExpectScreen("the prompt on one row", func(v *vtScreen) bool { return v.RowOf("> "+text) >= 0 })
	s.Resize(40, 40)
	s.Type("Z")
	s.ExpectScreen("the prompt rewrapped at 40 columns", func(v *vtScreen) bool {
		first := v.RowOf("> " + text[:38])
		return first >= 0 && v.RowOf(text[38:]+"Z") == first+1
	})
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, text+"Z") {
		t.Errorf("request does not carry the whole prompt: %s", req)
	}
	s.Quit()
}

// TestPromptMarks checks the OSC 133 marks a terminal uses to find the prompt: prompt start
// and input start around "> ", and output start once the prompt is sent. Accessibility mode
// writes none.
func TestPromptMarks(t *testing.T) {
	t.Parallel()
	t.Run("terminal", func(t *testing.T) {
		t.Parallel()
		_, s := start(t, newProvider(t))
		s.Type("hi\r")
		s.Expect("ok")
		s.ExpectPrompt()
		raw := s.Raw()
		if !strings.Contains(raw, "\x1b]133;A\x07> \x1b]133;B\x07") {
			t.Errorf("prompt is not wrapped in prompt-start and input-start marks")
		}
		if i, c := strings.Index(raw, "hi"), strings.Index(raw, "\x1b]133;C\x07"); c < i {
			t.Errorf("output-start mark missing or before the submitted prompt")
		}
		s.Quit()
	})
	t.Run("accessible", func(t *testing.T) {
		t.Parallel()
		_, s := start(t, newProvider(t), "--accessible")
		s.Type("hi\r")
		s.Expect("ok")
		s.ExpectPrompt()
		s.Quit()
		if strings.Contains(s.Raw(), "\x1b]133;") {
			t.Errorf("accessibility mode wrote OSC 133 marks")
		}
	})
}

// TestTurnDivider checks that a finished turn is set off from the next prompt by a blank line
// and a rule of at most 80 columns, plain dashes in accessibility mode, and that the prompt after
// the banner or a command that is not a turn gets none.
func TestTurnDivider(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		args  []string
		glyph string
	}{{"terminal", nil, "─"}, {"accessible", []string{"--accessible"}, "-"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newProvider(t)
			_, s := start(t, p, tc.args...)
			s.Type("/shortcuts\r")
			s.Expect("Display this shortcuts")
			s.ExpectPrompt()
			rule := regexp.MustCompile(`^` + regexp.QuoteMeta(tc.glyph) + `{20,}$`)
			for _, line := range s.ScreenState().Lines() {
				if rule.MatchString(line) {
					t.Fatalf("divider before any turn: %q", line)
				}
			}

			p.Queue(say("answer one"))
			s.Type("question one\r")
			s.Expect("answer one")
			s.ExpectPrompt()
			lines := s.ScreenState().Lines()
			i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "> question one") })
			j := slices.IndexFunc(lines[max(i, 0):], func(l string) bool { return strings.HasPrefix(l, "[tokens:") })
			if i < 0 || j < 0 || i+j+3 >= len(lines) {
				t.Fatalf("turn or footer not found:\n%s", strings.Join(lines, "\n"))
			}
			after := lines[i+j+1 : i+j+4]
			if after[0] != "" || !rule.MatchString(after[1]) || len([]rune(after[1])) > 80 || !strings.HasPrefix(after[2], "[model:") {
				t.Errorf("after the footer: %q, want a blank line, a rule of at most 80 %q and the model line", after, tc.glyph)
			}
			s.Quit()
		})
	}
}
