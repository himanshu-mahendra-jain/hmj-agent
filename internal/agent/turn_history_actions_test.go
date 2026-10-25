package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/terminal/theme"
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
// lands on the clipboard, without the response's escape sequence, and what is printed.
func TestRunTurnActionCopiesAndViews(t *testing.T) {
	turn := &Turn{Index: 2, Prompt: "why?", Response: "because\x1b[2J"}
	cases := []struct {
		action    string
		clipboard string // "" when nothing is copied
		printed   string
	}{
		{"copy_output", "because", "copied AI output of turn [2/3] to clipboard"},
		{"copy_prompt", "why?", "copied prompt of turn [2/3] to clipboard"},
		{"copy_both", "## User\n\nwhy?\n\n## Assistant\n\nbecause\n", "copied full turn [2/3] to clipboard (Markdown)"},
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
	turns := turnsOf(conv.Messages)
	if len(turns) != 1 {
		t.Fatalf("turns = %+v", turns)
	}
	want := "⚙ list_files: {}\n\n│ list_files: a.go\n\n│ tool: lost"
	if turns[0].Response != want {
		t.Errorf("response = %q, want %q", turns[0].Response, want)
	}
}

// TestHistoryShowsCompactedTurns checks that /history lists the turns a compaction replaced,
// marked as compacted, before the turns since, numbered as one list, and that a compacted turn
// can be viewed. The compaction itself adds nothing to the last compacted turn.
func TestHistoryShowsCompactedTurns(t *testing.T) {
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "question 1"}, {Role: "assistant", Content: "answer 1"},
		{Role: "user", Content: "question 2"}, {Role: "assistant", Content: "answer 2"},
	}}
	compaction := chat.Message{Role: "assistant", Blocks: []chat.Block{{Type: chat.BlockSummary, Text: "summary"}}}
	if err := conv.ApplyCompaction(compaction, len(conv.Messages)); err != nil {
		t.Fatal(err)
	}
	conv.Messages = append(conv.Messages, chat.Message{Role: "user", Content: "question 3"}, chat.Message{Role: "assistant", Content: "answer 3"})

	turns, err := HistoryTurns(conv)
	if err != nil || len(turns) != 3 {
		t.Fatalf("HistoryTurns = %+v (%v), want 3 turns", turns, err)
	}
	if !turns[0].Compacted || !turns[1].Compacted || turns[2].Compacted || turns[2].Index != 3 || turns[1].Response != "answer 2" {
		t.Errorf("turns = %+v, want turns 1 and 2 compacted and turn 3 not", turns)
	}
	items := buildTurnItems(turns)
	if items[0].Text != "[1/3] (compacted) question 1" || items[2].Text != "[3/3] question 3" {
		t.Errorf("labels = %q, %q", items[0].Text, items[2].Text)
	}
	if got := turnsOf(conv.Messages); len(got) != 1 || got[0].Prompt != "question 3" {
		t.Errorf("turnsOf = %+v, want only the turn in the conversation", got)
	}

	out := captureOutput(t, func() {
		if err := HandleHistory(conv, lines("1\n4\n")); err != nil {
			t.Errorf("HandleHistory: %v", err)
		}
	})
	if !strings.Contains(out, "question 1") || !strings.Contains(out, "answer 1") {
		t.Errorf("viewing the compacted turn printed %q", out)
	}
}

// TestHistoryReportsAnUnreadableArchive checks that /history says when the compacted turns
// cannot be read and still lists the turns since.
func TestHistoryReportsAnUnreadableArchive(t *testing.T) {
	conv := &session.Conversation{
		HistoryPath:   filepath.Join(t.TempDir(), "missing.jsonl"),
		ArchivedBytes: 10,
		Messages:      []chat.Message{{Role: "user", Content: "question 3"}, {Role: "assistant", Content: "answer 3"}},
	}
	out := captureOutput(t, func() {
		if err := HandleHistory(conv, lines("1\n4\n")); err != nil {
			t.Errorf("HandleHistory: %v", err)
		}
	})
	if !strings.Contains(out, "[could not read all the turns before the compaction:") || !strings.Contains(out, "answer 3") {
		t.Errorf("output %q should report the archive and show the live turn", out)
	}
}

// TestHistoryViewRendersOnlyTheModelsText checks that viewing a turn renders the model's
// Markdown but shows tool calls and results as written, so a "# comment" in a file it read is
// not drawn as a heading.
func TestHistoryViewRendersOnlyTheModelsText(t *testing.T) {
	enabled := theme.Enabled()
	theme.SetEnabled(true)
	t.Cleanup(func() { theme.SetEnabled(enabled) })
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "read it"},
		{Role: "assistant", Content: "**Reading**", ToolCalls: []chat.ToolCall{{ID: "c1", Function: chat.ToolCallFunction{Name: "read_file", Arguments: `{"path":"x.sh"}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: "# not a heading\n- not a bullet"},
		{Role: "assistant", Content: "- done"},
	}}
	turns := turnsOf(conv.Messages)
	if len(turns) != 1 {
		t.Fatalf("turns = %+v", turns)
	}
	got := turns[0].viewText()
	if !strings.Contains(got, theme.Markdown("Reading", theme.MarkdownBold)) || !strings.Contains(got, "• done") {
		t.Errorf("model text not rendered: %q", got)
	}
	if !strings.Contains(got, "# not a heading\n- not a bullet") {
		t.Errorf("tool result changed: %q", got)
	}
	if strings.Contains(turns[0].Response, "\x1b") || !strings.Contains(turns[0].Response, "**Reading**") {
		t.Errorf("Response, which is copied, should stay raw: %q", turns[0].Response)
	}
}
