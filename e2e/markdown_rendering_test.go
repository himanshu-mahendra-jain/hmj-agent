package e2e

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// markdownAnswer is an answer in Markdown, and rendered is how it reads on screen once rendered,
// each line after the 2-column margin.
const (
	markdownAnswer = "## Findings\n- **first** item\n- second with `code`\nplain end"
	rendered       = `  Findings\r?\n  • first item\r?\n  • second with code\r?\n  plain end`
)

// TestMarkdownIsRendered checks that the model's Markdown is rendered as it streams, in the
// transcript of a resumed conversation and in /history's view of a turn.
func TestMarkdownIsRendered(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e, s := start(t, p)
	p.Queue(say(markdownAnswer))
	s.Type("summarize\r")
	s.ExpectRegexp(rendered)
	s.ExpectPrompt()
	if strings.Contains(s.Screen(), "**first**") || strings.Contains(s.Screen(), "## Findings") {
		t.Errorf("markers left in the streamed answer:\n%s", s.Screen())
	}
	s.Quit()

	s = e.Start()
	s.Expect("resumed conversation")
	s.ExpectRegexp(rendered)
	s.ExpectPrompt()

	s.Type("\x19") // Ctrl+Y
	s.Expect("browse turns")
	s.Type("\r")
	s.Expect("choose action")
	s.TypeSlowly("view")
	s.Type("\r")
	s.ExpectRegexp(rendered)
	s.ExpectPrompt()
	if strings.Contains(s.Screen(), "**first**") {
		t.Errorf("markers left in the resumed transcript or the turn view:\n%s", s.Screen())
	}
	s.Quit()
}

// TestMarkdownWithoutColorOrAccessible checks that with colour off the answer is shown as
// written, and that accessibility mode renders it in ASCII, with no margin, without moving the
// cursor.
func TestMarkdownWithoutColorOrAccessible(t *testing.T) {
	t.Parallel()
	t.Run("no color", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		_, s := start(t, p, "--no-color")
		p.Queue(say(markdownAnswer))
		s.Type("summarize\r")
		s.Expect("## Findings")
		s.Expect("- **first** item")
		s.ExpectPrompt()
		s.Quit()
	})
	t.Run("accessible", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		_, s := start(t, p, "--accessible")
		p.Queue(say(markdownAnswer))
		s.Type("summarize\r")
		// No margin or wrapping: a screen reader reads each line as it comes.
		s.ExpectRegexp(`\nFindings\r?\n- first item\r?\n- second with code\r?\nplain end`)
		s.ExpectPrompt()
		s.Quit()
		if m := regexp.MustCompile(`\x1b\[[0-9;]*[ABCDEFGHJKST]`).FindString(s.Raw()); m != "" {
			t.Errorf("accessible mode wrote cursor control sequence %q", m)
		}
	})
}

// TestLongAnswerWrapsWithAMargin checks that a long answer is wrapped to the window, each line
// after the margin, and a list item's wrapped lines hang under its text.
func TestLongAnswerWrapsWithAMargin(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	words := strings.TrimSpace(strings.Repeat("alpha beta gamma delta ", 12))
	p.Queue(say(words + "\n- " + words + " END"))
	s.Type("go\r")
	s.Expect("END")
	s.ExpectPrompt()
	lines := s.ScreenState().Lines()
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "  alpha") })
	end := slices.IndexFunc(lines, func(l string) bool { return strings.HasSuffix(l, "END") })
	if start < 0 || end <= start+2 {
		t.Fatalf("answer not found or not wrapped:\n%s", strings.Join(lines, "\n"))
	}
	inItem := false
	for _, l := range lines[start : end+1] {
		inItem = inItem || strings.HasPrefix(l, "  • ")
		switch {
		case utf8.RuneCountInString(l) > 119:
			t.Errorf("line over the window: %q", l)
		case inItem && !strings.HasPrefix(l, "  • ") && !strings.HasPrefix(l, "    "):
			t.Errorf("wrapped item line does not hang under its text: %q", l)
		case !strings.HasPrefix(l, "  "):
			t.Errorf("line lacks the margin: %q", l)
		}
	}
}
