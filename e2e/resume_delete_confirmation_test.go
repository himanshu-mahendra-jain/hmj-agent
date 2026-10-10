package e2e

import (
	"strings"
	"testing"
)

// startWithTwoConversations starts hmj and leaves it with two conversations, [1] and the
// active [2], at the prompt.
func startWithTwoConversations(t *testing.T) (*provider, *session) {
	t.Helper()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(say("first answer"), say("second answer"))
	s.Type("first chat\r")
	s.Expect("first answer")
	s.ExpectPrompt()
	s.Type("\x0e") // Ctrl+N
	s.Expect("created conversation")
	s.ExpectPrompt()
	s.Type("second chat\r")
	s.Expect("second answer")
	s.ExpectPrompt()
	return p, s
}

// highlightedRow returns the menu row marked with the selection glyph, or "".
func highlightedRow(v *vtScreen) string {
	for _, l := range v.Lines() {
		if strings.Contains(l, "❯") {
			return l
		}
	}
	return ""
}

// TestResumeDeleteCancelledByArrowKey checks that an arrow key at the delete confirmation
// cancels it: nothing is deleted, the menu says so, and the arrow does not also move the
// highlight.
func TestResumeDeleteCancelledByArrowKey(t *testing.T) {
	t.Parallel()
	for _, arrow := range []struct{ name, keys string }{{"down", "\x1b[B"}, {"up", "\x1b[A"}, {"left", "\x1b[D"}} {
		t.Run(arrow.name, func(t *testing.T) {
			t.Parallel()
			_, s := startWithTwoConversations(t)
			s.Type("\x12") // Ctrl+R
			s.Expect("2/2 matches")
			s.Type("\x04") // Ctrl+D
			s.Expect("[y/N]")
			s.Type(arrow.keys)
			s.Expect("delete cancelled")
			v := s.ExpectScreen("the menu with [2] still highlighted", func(v *vtScreen) bool {
				return strings.Contains(highlightedRow(v), "[2]") && v.RowOf("delete cancelled") >= 0
			})
			if v.RowOf("[1]") < 0 {
				t.Errorf("conversation [1] missing from the menu:\n%s", v.Dump())
			}
			if v.RowOf("[y/N]") >= 0 {
				t.Errorf("the answered confirmation is still on screen:\n%s", v.Dump())
			}
			s.PressEsc()
			s.ExpectPrompt()
			s.Type("\x12")
			s.Expect("2/2 matches")
			s.PressEsc()
			s.ExpectPrompt()
			s.Quit()
		})
	}
}

// TestResumeDeleteDoublePressDeletesOnce checks that "yy" typed as one burst deletes the
// highlighted conversation once, without waiting for another key, and that the second y is
// discarded instead of landing in the menu's filter.
func TestResumeDeleteDoublePressDeletesOnce(t *testing.T) {
	t.Parallel()
	_, s := startWithTwoConversations(t)
	s.Type("\x12")
	s.Expect("2/2 matches")
	s.Type("\x04")
	s.Expect("[y/N]")
	s.Type("yy")
	s.Expect("deleted conversation")
	// [2] disappears from the menu, and the answered confirmation naming it is erased too.
	v := s.ExpectScreen("one conversation left with an empty filter", func(v *vtScreen) bool {
		return v.RowOf("[1]") >= 0 && v.RowOf("[2]") < 0 && v.RowOf("[y/N]") < 0
	})
	for _, l := range v.Lines() {
		if strings.TrimSpace(l) == "> y" {
			t.Errorf("the second y reached the filter:\n%s", v.Dump())
		}
	}
	s.PressEsc()
	s.ExpectPrompt()
	s.Type("\x12")
	s.Expect("1/1 matches")
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}

// TestResumeDeleteDeclinedByOtherKeys checks that Enter, n and Ctrl+D at the confirmation all
// keep the conversation.
func TestResumeDeleteDeclinedByOtherKeys(t *testing.T) {
	t.Parallel()
	_, s := startWithTwoConversations(t)
	s.Type("\x12")
	s.Expect("2/2 matches")
	for _, key := range []string{"\r", "n", "\x04"} {
		s.Type("\x04")
		s.Expect("[y/N]")
		s.Type(key)
		s.Expect("delete cancelled")
	}
	s.PressEsc()
	s.ExpectPrompt()
	s.Type("\x12")
	s.Expect("2/2 matches")
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}
