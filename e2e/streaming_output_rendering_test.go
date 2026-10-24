package e2e

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"hmj-agent/internal/terminal"
)

// assertNotHijacked fails when the screen was cleared (the banner scrolled away is fine, a
// cleared screen is not) or an OSC string carrying marker reached the terminal.
func assertNotHijacked(t *testing.T, s *session, marker string) {
	t.Helper()
	v := s.ScreenState()
	if v.RowOf("[model: mock/model-a · effort: default]") < 0 {
		t.Errorf("the screen was cleared:\n%s", v.Dump())
	}
	for _, osc := range v.oscs {
		if strings.Contains(osc, marker) {
			t.Errorf("an OSC string from the model reached the terminal: %q", osc)
		}
	}
	if strings.Contains(s.Raw(), "\x1b[2J\x1b[H"+marker) {
		t.Errorf("a screen-clearing sequence from the model reached the terminal")
	}
}

// TestModelControlSequencesAreSanitized has the model reply with escape sequences that
// would clear the screen, move the cursor, set the window title or rewrite the line, both
// whole and split across stream chunks. They are shown as harmless text or dropped.
func TestModelControlSequencesAreSanitized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer reply
		want   string // text that must be on screen afterwards
	}{
		{"whole sequences", say("safe\x1b[2J\x1b[Hpwn1\x1b]0;pwn1 title\x07 text\rover\x08\x07 end\n"), "text"},
		{"split escape sequence", sayDeltas("split \x1b", "[2J\x1b[Hpwn2 ", "\x1b]0;pwn2", " title\x07done\n"), "done"},
		{"C1 control characters", say("c1 \u009b2J\u009d0;pwn3\u009c tail\n"), "tail"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newProvider(t)
			_, s := start(t, p)
			p.Queue(tc.answer)
			s.Type("go\r")
			s.Expect(tc.want)
			s.ExpectPrompt()
			for _, m := range []string{"pwn1", "pwn2", "pwn3"} {
				assertNotHijacked(t, s, m)
			}
			// The prompt still works where the screen says it is.
			s.Type("next\r")
			s.Expect("ok")
			s.ExpectPrompt()
			s.Quit()
		})
	}
}

// TestToolOutputControlSequencesAreSanitized runs a command whose output clears the screen,
// sets the title and draws a carriage-return progress bar. The tool result shown is plain.
func TestToolOutputControlSequencesAreSanitized(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(callTool("run_command", map[string]any{"command": `printf 'begin\033[2J\033[Hpwn4\033]0;pwn4 title\007 10%%\r100%%\n'`}), say("finished"))
	s.Type("run it\r")
	s.Expect("finished")
	s.ExpectPrompt()
	assertNotHijacked(t, s, "pwn4")
	s.Quit()
}

// TestResizeWhileStreaming narrows the terminal while a reply is streaming, halts it with
// Esc, and checks that the next prompt wraps at the new width.
func TestResizeWhileStreaming(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 100
	e.WriteConfig(p.config("model-a"))
	s := e.Start()
	s.ExpectPrompt()
	p.Queue(streamForever("tick", 20*time.Millisecond))
	s.Type("stream\r")
	s.Expect("tick")
	s.Resize(30, 40)
	s.Expect("tick")
	s.PressEsc()
	s.Expect("[halted by user (Esc)]")
	s.ExpectPrompt()
	text := strings.Repeat("0123456789", 5)
	s.Type(text)
	s.ExpectScreen("the prompt wrapped at 40 columns", func(v *vtScreen) bool {
		first := v.RowOf("> " + text[:38])
		return first >= 0 && v.RowOf(text[38:]) == first+1
	})
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()
}

// tableRowsAfter returns the rows of the screen below the last row holding marker that
// belong to a drawn table.
func tableRowsAfter(v *vtScreen, marker string) []string {
	var rows []string
	for _, l := range v.Lines()[v.RowOf(marker)+1:] {
		if first, _ := utf8.DecodeRuneInString(strings.TrimSpace(l)); strings.ContainsRune("┌├│└", first) {
			rows = append(rows, l)
		}
	}
	return rows
}

// assertTableAligned checks that every table row below marker has the same display width, at most cols,
// and that the right border of each row is in the same column.
func assertTableAligned(t *testing.T, v *vtScreen, marker string, cols int) {
	t.Helper()
	rows := tableRowsAfter(v, marker)
	if len(rows) < 5 {
		t.Fatalf("no table on screen:\n%s", v.Dump())
	}
	want := terminal.VisibleWidth(rows[0])
	for _, r := range rows {
		w := terminal.VisibleWidth(r)
		if w != want {
			t.Errorf("table row %q is %d columns wide, the top border %d\n%s", r, w, want, v.Dump())
		}
		if w > cols {
			t.Errorf("table row %q is %d columns wide, wider than the %d-column terminal", r, w, cols)
		}
		last, _ := utf8.DecodeLastRuneInString(strings.TrimRight(r, " "))
		if !strings.ContainsRune("┐┤│┘", last) {
			t.Errorf("table row %q does not end with a border", r)
		}
	}
}

// TestResizeWhileTableStreams narrows the terminal after the header of a table has
// streamed but before its rows arrive. The table is drawn once complete, so it must fit the
// new width, with every row aligned.
func TestResizeWhileTableStreams(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 120
	e.WriteConfig(p.config("model-a"))
	s := e.Start()
	s.ExpectPrompt()
	release := make(chan struct{})
	header := "Intro line\n| Name | Description |\n|---|---|\n"
	long := strings.Repeat("a long description that needs wrapping ", 3)
	rows := "| alpha | " + long + "|\n| beta | " + long + "|\n\nAfter the table\n"
	p.Queue(sayAfter(header, release, rows))
	s.Type("table please\r")
	s.Expect("Intro line")
	s.Resize(40, 60)
	close(release)
	s.Expect("After the table")
	s.ExpectPrompt()
	assertTableAligned(t, s.ScreenState(), "Intro line", 60)
	s.Quit()
}

// TestTableWithWideAndCombiningCharacters streams a table whose cells hold CJK text, emoji
// and letters with combining accents. Every row must line up with the borders.
func TestTableWithWideAndCombiningCharacters(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 70
	e.WriteConfig(p.config("model-a"))
	s := e.Start()
	s.ExpectPrompt()
	table := "| 名前 | Status | Note |\n|:---|:---:|---:|\n" +
		"| 日本語テキスト | 🚀 launched | café au lait |\n" +
		"| ascii | ✅ done | \U0001fae0 melting face with a long note that wraps around |\n" +
		"| 한국어 | ⭐ star | näive |\n\nend of table\n"
	p.Queue(sayDeltas(table[:30], table[30:90], table[90:]))
	s.Type("table\r")
	s.Expect("end of table")
	s.ExpectPrompt()
	assertTableAligned(t, s.ScreenState(), "> table", 70)
	s.Quit()
}

// TestTableWithMarkdownInCells streams a table with bold headers, a stray asterisk, an
// unclosed code span and bold text long enough to wrap inside its cell. The markers are not
// drawn, and every row still lines up with the borders.
func TestTableWithMarkdownInCells(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 60
	e.WriteConfig(p.config("model-a"))
	s := e.Start()
	s.ExpectPrompt()
	p.Queue(say("| **Name** | **Note** |\n|---|---|\n| calc | 5 * 3 |\n| code | `unclosed span |\n" +
		"| long | **bold words that keep going well past the width of this column** |\n\nend of table\n"))
	s.Type("table\r")
	s.Expect("end of table")
	s.ExpectPrompt()
	v := s.ScreenState()
	assertTableAligned(t, v, "> table", 60)
	if v.RowOf("**Name**") >= 0 || v.RowOf("Name") < 0 {
		t.Errorf("the bold header is drawn with its markers or not at all:\n%s", v.Dump())
	}
	s.Quit()
}
