package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/session"
)

// fakeClipboard puts an xclip on PATH that stores what it is given in a file, and returns
// that file's path. WAYLAND_DISPLAY is cleared so xclip is the utility chosen.
func fakeClipboard(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store := filepath.Join(dir, "clipboard")
	script := "#!/bin/sh\nexec /bin/cat > '" + store + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "xclip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "")
	return store
}

// TestRunTurnActionCopiesAndViews checks every action of the /history turn browser: what
// lands on the clipboard and what is printed.
func TestRunTurnActionCopiesAndViews(t *testing.T) {
	turn := &Turn{Index: 2, Prompt: "why?", Response: "because\x1b[2J"}
	cases := []struct {
		action    string
		clipboard string // "" when nothing is copied
		printed   string
	}{
		{"copy_output", "because\x1b[2J", "copied AI output of turn [2/3] to clipboard"},
		{"copy_prompt", "why?", "copied prompt of turn [2/3] to clipboard"},
		{"copy_both", "## User\n\nwhy?\n\n## Assistant\n\nbecause\x1b[2J\n", "copied full turn [2/3] to clipboard (Markdown)"},
		{"view", "", "> you:"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			store := fakeClipboard(t)
			out := captureOutput(t, func() {
				if err := runTurnAction(tc.action, turn, 3); err != nil {
					t.Errorf("runTurnAction: %v", err)
				}
			})
			if !strings.Contains(out, tc.printed) {
				t.Errorf("output %q lacks %q", out, tc.printed)
			}
			data, err := os.ReadFile(store)
			if tc.clipboard == "" {
				if err == nil {
					t.Errorf("view copied %q to the clipboard", data)
				}
				if !strings.Contains(out, "because") || strings.Contains(out, "\x1b[2J") {
					t.Errorf("view must show the response without its escape sequences: %q", out)
				}
				return
			}
			if string(data) != tc.clipboard {
				t.Errorf("clipboard = %q, want %q", data, tc.clipboard)
			}
		})
	}
}

// TestRunTurnActionClipboardUnavailable checks that each copy action reports a missing
// clipboard instead of claiming success.
func TestRunTurnActionClipboardUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("WAYLAND_DISPLAY", "")
	for _, action := range []string{"copy_output", "copy_prompt", "copy_both"} {
		var err error
		out := captureOutput(t, func() { err = runTurnAction(action, &Turn{Index: 1, Prompt: "p", Response: "r"}, 1) })
		if err == nil || !strings.Contains(err.Error(), "clipboard unavailable") {
			t.Errorf("%s: err = %v, want clipboard unavailable", action, err)
		}
		if strings.Contains(out, "copied") {
			t.Errorf("%s: printed success %q", action, out)
		}
	}
}

// TestHandleHistoryActionMenuClosed checks that leaving the action menu after choosing a
// turn does nothing when cancelled and returns the error when input ends.
func TestHandleHistoryActionMenuClosed(t *testing.T) {
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "first prompt"},
		{Role: "assistant", Content: "first answer"},
	}}
	store := fakeClipboard(t)

	out := captureOutput(t, func() {
		if err := HandleHistory(conv, lines("1\n\n")); err != nil {
			t.Errorf("cancelled action menu: %v", err)
		}
	})
	if strings.Contains(out, "copied") || strings.Contains(out, "first answer") {
		t.Errorf("a cancelled action menu acted: %q", out)
	}
	if _, err := os.Stat(store); err == nil {
		t.Error("a cancelled action menu copied to the clipboard")
	}

	useAccessibility(t)
	var err error
	captureOutput(t, func() { err = HandleHistory(conv, lines("1\n")) })
	if err != nil {
		t.Errorf("err = %v, want the action menu cancelled when input ends", err)
	}
}

// TestTurnBrowserHelpers checks the turn labels and lookups of the /history browser.
func TestTurnBrowserHelpers(t *testing.T) {
	long := strings.Repeat("é", 70)
	if got := turnSnippet("  line one\nline two  ", 60); got != "line one line two" {
		t.Errorf("turnSnippet joined lines = %q", got)
	}
	if got := turnSnippet(long, 60); got != strings.Repeat("é", 57)+"..." {
		t.Errorf("turnSnippet cut = %q, want 57 runes and ...", got)
	}

	turns := []Turn{{Index: 1, Prompt: "a"}, {Index: 2, Prompt: "b"}}
	if got := findTurn(turns, "2"); got == nil || got.Prompt != "b" {
		t.Errorf("findTurn(2) = %+v", got)
	}
	if got := findTurn(turns, "3"); got != nil {
		t.Errorf("findTurn(3) = %+v, want nil", got)
	}
	items := buildTurnItems(turns)
	if len(items) != 2 || items[1].ID != "2" || items[1].Text != "[2/2] b" {
		t.Errorf("buildTurnItems = %+v", items)
	}
}

// TestExtractTurnsNamesUnmatchedToolResults checks that a tool result whose call is not in
// the transcript is still shown, under a generic name, and that a call without arguments
// shows "{}".
func TestExtractTurnsNamesUnmatchedToolResults(t *testing.T) {
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "tool", ToolCallID: "before-any-turn", Content: "ignored"},
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "c1", Function: chat.ToolCallFunction{Name: "list_files"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "a.go\n"},
		{Role: "tool", ToolCallID: "orphan", Content: " lost "},
	}}
	turns := ExtractTurns(conv)
	if len(turns) != 1 {
		t.Fatalf("turns = %+v", turns)
	}
	want := "⚙ list_files: {}\n\n│ list_files: a.go\n\n│ tool: lost"
	if turns[0].Response != want {
		t.Errorf("response = %q, want %q", turns[0].Response, want)
	}
	if ExtractTurns(nil) != nil {
		t.Error("ExtractTurns(nil) should be nil")
	}
}
