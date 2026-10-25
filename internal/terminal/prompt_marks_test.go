package terminal

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"hmj-agent/v2/internal/terminal/theme"
)

// TestPromptMarksWrapPromptAndOutput checks the OSC 133 marks on a real terminal: prompt start
// and input start around the "> " glyph, a secondary-prompt mark on continuation lines, and the
// output-start mark once the prompt is submitted.
func TestPromptMarksWrapPromptAndOutput(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	screen := newPTYScreen(t)
	screen.setRaw(t)
	ch := readSeamless(bufio.NewReader(screen.slave))

	// "a", then Enter with "b" already pending: the newline is inserted, giving a second line.
	screen.Type(t, "a\rb")
	screen.waitFor(t, "b")
	screen.Type(t, "\r")
	if r := <-ch; r.text != "a\nb" {
		t.Fatalf("got %q, want %q", r.text, "a\nb")
	}

	// The output-start mark is the last thing written; wait for the reader to collect it.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(screen.Raw(), markOutputStart) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	raw := screen.Raw()
	for _, want := range []string{
		markPromptStart + "> " + markInputStart,
		markSecondaryStart + "  " + markInputStart,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("output lacks %q:\n%q", want, raw)
		}
	}
	if out, in := strings.LastIndex(raw, markOutputStart), strings.LastIndex(raw, markInputStart); out < in {
		t.Errorf("output-start mark missing or before the last input mark:\n%q", raw)
	}
}

// TestPromptPrefixMarks checks when PromptPrefix carries the marks: on a terminal, but not in
// accessibility mode, on the Linux console, on a dumb terminal or when output is not a terminal.
func TestPromptPrefixMarks(t *testing.T) {
	theme.SetEnabled(false)
	defer theme.SetAccessibility(false)

	t.Run("not a terminal", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		if got := PromptPrefix(); got != "> " {
			t.Errorf("PromptPrefix() = %q, want plain %q", got, "> ")
		}
	})

	screen := newPTYScreen(t) // stdout is now a terminal
	_ = screen
	cases := []struct {
		name       string
		term       string
		accessible bool
		want       string
	}{
		{"terminal", "xterm-256color", false, markPromptStart + "> " + markInputStart},
		{"accessibility mode", "xterm-256color", true, "> "},
		{"linux console", "linux", false, "> "},
		{"dumb terminal", "dumb", false, "> "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			theme.SetAccessibility(tc.accessible)
			if got := PromptPrefix(); got != tc.want {
				t.Errorf("PromptPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}
