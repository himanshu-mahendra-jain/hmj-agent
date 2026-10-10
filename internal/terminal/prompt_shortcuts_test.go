package terminal

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPromptShortcutsOnTerminal types every prompt key into ReadSeamlessInput on a pseudo
// terminal, byte for byte as a terminal sends it, and checks what the prompt returns. It
// covers the keys the shortcuts table lists and the standard shell editing keys it leaves out.
// Each element of keys is typed after a pause, so a lone Esc is not read as the start of an
// escape sequence (Alt+key) together with the keys after it.
func TestPromptShortcutsOnTerminal(t *testing.T) {
	const (
		left     = "\x1b[D"
		ctrlLeft = "\x1b[1;5D"
		paste    = "\x1b[200~"
		pasteEnd = "\x1b[201~"
	)
	tests := []struct {
		name       string
		history    []string
		keys       []string
		wantText   string
		wantAction ShortcutAction
		wantErr    error
	}{
		// Typing and submitting
		{name: "Enter submits", keys: []string{"hello\r"}, wantText: "hello"},
		{name: "newline submits", keys: []string{"hello\n"}, wantText: "hello"},
		{name: "UTF-8 text", keys: []string{"é日\r"}, wantText: "é日"},
		{name: "Tab is typed", keys: []string{"a\tb\r"}, wantText: "a\tb"},
		{name: "Backspace", keys: []string{"abc\x7f\r"}, wantText: "ab"},
		{name: "Ctrl+H is Backspace", keys: []string{"abc\x08\r"}, wantText: "ab"},
		{name: "unbound control keys are ignored", keys: []string{"a\x02\x0c\x14b\r"}, wantText: "ab"},

		// Cursor movement
		{name: "Left", keys: []string{"ac" + left + "b\r"}, wantText: "abc"},
		{name: "Left in application mode", keys: []string{"ac\x1bODb\r"}, wantText: "abc"},
		{name: "Right", keys: []string{"ac" + left + left + "\x1b[Cb\r"}, wantText: "abc"},
		{name: "Right in application mode", keys: []string{"ac" + left + left + "\x1bOCb\r"}, wantText: "abc"},
		{name: "Home", keys: []string{"bc\x1b[Ha\r"}, wantText: "abc"},
		{name: "Home as ESC [1~", keys: []string{"bc\x1b[1~a\r"}, wantText: "abc"},
		{name: "Home in application mode", keys: []string{"bc\x1bOHa\r"}, wantText: "abc"},
		{name: "End", keys: []string{"bc\x1b[Ha\x1b[Fd\r"}, wantText: "abcd"},
		{name: "End as ESC [4~", keys: []string{"bc\x1b[Ha\x1b[4~d\r"}, wantText: "abcd"},
		{name: "End in application mode", keys: []string{"bc\x1b[Ha\x1bOFd\r"}, wantText: "abcd"},
		{name: "Ctrl+A", keys: []string{"world\x01hello \r"}, wantText: "hello world"},
		{name: "Ctrl+E", keys: []string{"world\x01hello \x05!\r"}, wantText: "hello world!"},
		{name: "Up and Down move between lines", keys: []string{paste + "ab\ncd" + pasteEnd, "\x1b[AX\x1b[BY\r"}, wantText: "abX\ncdY"},
		{name: "Ctrl+Left", keys: []string{"one two" + ctrlLeft + "_\r"}, wantText: "one _two"},
		{name: "Alt+Left", keys: []string{"one two\x1b[1;3D_\r"}, wantText: "one _two"},
		{name: "Alt+B", keys: []string{"one two\x1bb_\r"}, wantText: "one _two"},
		{name: "Ctrl+Right", keys: []string{"one two\x01\x1b[1;5C_\r"}, wantText: "one_ two"},
		{name: "Alt+Right", keys: []string{"one two\x01\x1b[1;3C_\r"}, wantText: "one_ two"},
		{name: "Alt+F", keys: []string{"one two\x01\x1bf_\r"}, wantText: "one_ two"},
		{name: "word keys skip punctuation", keys: []string{"src/main.go" + ctrlLeft + ctrlLeft + "_\r"}, wantText: "src/_main.go"},

		// Deleting
		{name: "Delete", keys: []string{"abc" + left + "\x1b[3~\r"}, wantText: "ab"},
		{name: "Ctrl+U", keys: []string{"drop me\x15kept\r"}, wantText: "kept"},
		{name: "Ctrl+K", keys: []string{"keepdrop" + strings.Repeat(left, 4) + "\x0b\r"}, wantText: "keep"},
		{name: "Ctrl+W deletes back to whitespace", keys: []string{"cat src/main.go\x17x\r"}, wantText: "cat x"},
		{name: "Alt+Backspace deletes back to punctuation", keys: []string{"cat src/main.go\x1b\x7f\r"}, wantText: "cat src/main."},
		{name: "Alt+Backspace held down", keys: []string{"one two three\x1b\x7f\x1b\x7fx\r"}, wantText: "one x"},
		{name: "Alt+Backspace as ESC Ctrl+H", keys: []string{"one two\x1b\x08x\r"}, wantText: "one x"},
		{name: "Alt+D", keys: []string{"one two\x01\x1bdx\r"}, wantText: "x two"},
		{name: "Ctrl+Delete", keys: []string{"one two\x01\x1b[3;5~x\r"}, wantText: "x two"},
		{name: "Alt+Delete", keys: []string{"one two\x01\x1b[3;3~x\r"}, wantText: "x two"},
		{name: "Esc wipes the prompt", keys: []string{"draft", "\x1b", "new\r"}, wantText: "new"},
		{name: "Ctrl+C with text clears it", keys: []string{"draft\x03new\r"}, wantText: "new"},
		{name: "Ctrl+C on an empty prompt is ignored", keys: []string{"\x03hello\r"}, wantText: "hello"},

		// Prompt history
		{name: "Up recalls history", history: []string{"older", "newest"}, keys: []string{"\x1b[A\x1b[A\r"}, wantText: "older"},
		{name: "Down restores the draft", history: []string{"older"}, keys: []string{"draft\x1b[A\x1b[B!\r"}, wantText: "draft!"},

		// Keys that end the prompt
		{name: "Ctrl+D exits", keys: []string{"\x04"}, wantErr: io.EOF},
		{name: "Ctrl+N", keys: []string{"\x0e"}, wantAction: ActionNew},
		{name: "Ctrl+R", keys: []string{"\x12"}, wantAction: ActionResume},
		{name: "Ctrl+O", keys: []string{"\x0f"}, wantAction: ActionModels},
		{name: "Ctrl+Y", keys: []string{"\x19"}, wantAction: ActionHistory},
		{name: "shortcut keys with text", keys: []string{"draft\x12"}, wantAction: ActionResume},
		{name: "slash opens the command menu", keys: []string{"/", "models\r"}, wantText: "/models"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			saved := GetPromptHistory()
			SetPromptHistory(tc.history)
			t.Cleanup(func() { SetPromptHistory(saved) })

			screen := newPTYScreen(t)
			ch := readSeamless(bufio.NewReader(os.Stdin))
			// Keys typed before raw mode is on would be handled by the line discipline.
			screen.waitFor(t, "\x1b[?2004h")
			for i, k := range tc.keys {
				if i > 0 {
					time.Sleep(100 * time.Millisecond)
				}
				screen.Type(t, k)
			}
			select {
			case r := <-ch:
				if r.text != tc.wantText || r.action != tc.wantAction || !errors.Is(r.err, tc.wantErr) || (r.err != nil) != (tc.wantErr != nil) {
					t.Errorf("got (%q, %v, %v), want (%q, %v, %v)", r.text, r.action, r.err, tc.wantText, tc.wantAction, tc.wantErr)
				}
			case <-time.After(3 * time.Second):
				screen.Type(t, "\x04") // end the prompt so the reader goroutine returns
				<-ch
				t.Fatalf("prompt did not end; screen: %q", screen.String())
			}
			if !screen.canonical(t) {
				t.Error("terminal mode not restored")
			}
		})
	}
}
