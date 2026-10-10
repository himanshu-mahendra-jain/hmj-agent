package e2e

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestHistoryCopiesPromptToClipboard copies a turn's prompt from /history and checks the OSC 52
// clipboard sequence the terminal receives. cleanEnv drops DISPLAY and WAYLAND_DISPLAY, so the
// developer's real clipboard is never touched.
func TestHistoryCopiesPromptToClipboard(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(say("the answer is 42"))
	s.Type("what is the answer\r")
	s.Expect("the answer is 42")
	s.ExpectPrompt()

	s.Type("\x19") // Ctrl+Y
	s.Expect("browse turns")
	s.Type("\r")
	s.Expect("choose action")
	s.TypeSlowly("user prompt")
	s.Expect("Copy User Prompt to clipboard")
	s.Type("\r")
	s.Expect("copied prompt of turn [1/1] to clipboard")
	s.ExpectPrompt()

	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("what is the answer")) + "\x07"
	if !strings.Contains(s.Raw(), want) {
		t.Errorf("no OSC 52 sequence carrying the prompt in the output")
	}
	s.Quit()
}
