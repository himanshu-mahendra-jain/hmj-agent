package agent

import (
	"fmt"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/diff"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTurns(t *testing.T) {
	// 1. Empty conversation
	conv := &session.Conversation{ID: "c1", Messages: nil}
	if turns := turnsOf(conv.Messages); len(turns) != 0 {
		t.Fatalf("expected 0 turns, got %d", len(turns))
	}

	// 2. Conversation with multiple turns and multi-part assistant responses
	conv.Messages = []chat.Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
		{Role: "user", Content: "What is 2+2?"},
		{Role: "assistant", Content: "2+2 is 4."},
		{Role: "assistant", Content: "Need anything else?"},
		{Role: "user", Content: "Single prompt without reply yet"},
	}

	turns := turnsOf(conv.Messages)
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}

	// Turn 1
	if turns[0].Index != 1 || turns[0].Prompt != "Hello" || turns[0].Response != "Hi there!" {
		t.Errorf("turn 0 mismatch: %+v", turns[0])
	}

	// Turn 2
	if turns[1].Index != 2 || turns[1].Prompt != "What is 2+2?" || turns[1].Response != "2+2 is 4.\n\nNeed anything else?" {
		t.Errorf("turn 1 mismatch: %+v", turns[1])
	}

	// Turn 3
	if turns[2].Index != 3 || turns[2].Prompt != "Single prompt without reply yet" || turns[2].Response != "" {
		t.Errorf("turn 2 mismatch: %+v", turns[2])
	}
}

func TestExtractTurnsIncludesToolsAndErrors(t *testing.T) {
	conv := &session.Conversation{
		ID: "test-turn-tools",
		Messages: []chat.Message{
			{Role: "user", Content: "Please check files"},
			{
				Role:    "assistant",
				Content: "Let me check the files.",
				ToolCalls: []chat.ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: chat.ToolCallFunction{
							Name:      "list_files",
							Arguments: `{"path":"."}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content:    "main.go\nREADME.md",
			},
			{
				Role:    "assistant",
				Content: "[error: HTTP 402: You're out of credits]",
			},
		},
	}

	turns := turnsOf(conv.Messages)
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}

	turn := turns[0]
	if turn.Prompt != "Please check files" {
		t.Errorf("unexpected prompt: %q", turn.Prompt)
	}

	// Response must contain text, tool call, tool result, and the error!
	if !strings.Contains(turn.Response, "Let me check the files.") {
		t.Errorf("turn response missing assistant text: %q", turn.Response)
	}
	if !strings.Contains(turn.Response, "⚙ list_files: {\"path\":\".\"}") {
		t.Errorf("turn response missing tool call: %q", turn.Response)
	}
	if !strings.Contains(turn.Response, "│ list_files: main.go\nREADME.md") {
		t.Errorf("turn response missing tool result: %q", turn.Response)
	}
	if !strings.Contains(turn.Response, "[error: HTTP 402: You're out of credits]") {
		t.Errorf("turn response missing error: %q", turn.Response)
	}
}

func TestDiffPrintDiffInline(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	vPath := filepath.Join(tmpDir, "v.txt")
	if err := os.WriteFile(vPath, []byte("const Version = \"v0.1.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	out := captureOutput(t, func() {
		printEditFileDiff(vPath, "const Version = \"v0.1.0\"", "const Version = \"v1\"")
	})

	// 1. Must NOT contain phantom spaces: "  v0.1.0  "
	if strings.Contains(out, "\"  v0.1.0  \"") || strings.Contains(out, "\" v0.1.0 \"") {
		t.Fatalf("found extra padding spaces in output:\n%s", out)
	}

	// 2. Must contain the version strings
	if !strings.Contains(out, "v0.1.0") || !strings.Contains(out, "v1") {
		t.Fatalf("missing version strings in output:\n%s", out)
	}

	// 3. Must contain reverse-video marks for inline token highlighting
	if !strings.Contains(out, "\x1b[7m") {
		t.Fatalf("missing reverse-video mark for inline token diff:\n%s", out)
	}

	// 4. Must contain row background colors (dark red bg for removed line, dark charcoal for added line)
	if !strings.Contains(out, "48;5;52") {
		t.Fatalf("missing dark red background for removed line:\n%s", out)
	}
	if !strings.Contains(out, "48;5;234") {
		t.Fatalf("missing dark charcoal background for added line:\n%s", out)
	}

	// 5. Test disabled mode has proper - and + glyphs and preserves indentation
	theme.SetEnabled(false)
	indentedPath := filepath.Join(tmpDir, "indented.go")
	if err := os.WriteFile(indentedPath, []byte("\t\tx := 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outPlain := captureOutput(t, func() {
		printEditFileDiff(indentedPath, "\t\tx := 1", "\t\tx := 2")
	})

	if !strings.Contains(outPlain, "- \t\tx := 1") {
		t.Fatalf("plain mode missing - glyph or lost indentation:\n%s", outPlain)
	}
	if !strings.Contains(outPlain, "+ \t\tx := 2") {
		t.Fatalf("plain mode missing + glyph or lost indentation:\n%s", outPlain)
	}
}

// Two changes far apart produce two hunks; the separator between them must be
// printed as "···" (or "..." in accessibility mode), not as a blank line.
func TestPrintDiffHunkSeparator(t *testing.T) {
	var oldLines, newLines []string
	for i := range 40 {
		line := fmt.Sprintf("line %d", i)
		oldLines = append(oldLines, line)
		switch i {
		case 2, 35:
			line += " changed"
		}
		newLines = append(newLines, line)
	}
	d := diff.Text("f.txt", strings.Join(oldLines, "\n")+"\n", strings.Join(newLines, "\n")+"\n", diff.Options{Context: 3})
	if len(d.Hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d", len(d.Hunks))
	}

	theme.SetEnabled(false)
	for _, tc := range []struct {
		accessible bool
		want       string
	}{{false, "  ···\n"}, {true, "  ...\n"}} {
		theme.SetAccessibility(tc.accessible)
		out := captureOutput(t, func() { printDiff(d) })
		theme.SetAccessibility(false)
		if !strings.Contains(out, tc.want) {
			t.Errorf("accessible=%v: separator %q missing:\n%s", tc.accessible, tc.want, out)
		}
		for l := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
			if strings.TrimSpace(l) == "" {
				t.Errorf("accessible=%v: blank line in diff output:\n%s", tc.accessible, out)
				break
			}
		}
	}
}
