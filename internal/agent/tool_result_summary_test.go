package agent

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
	"hmj-agent/internal/tools"
)

// TestSummarizeToolResult checks the few words each quiet tool's output becomes.
func TestSummarizeToolResult(t *testing.T) {
	search := "Search results for \"go\" from Brave (today is 2026-10-23):\n\n" +
		"1. [The Go Programming Language](https://go.dev/)\n   Go is an open source language.\n\n" +
		"2. [Go (game)](https://www.en.wikipedia.org/wiki/Go_(game))\n\n" +
		"To read the full text of any page, use fetch_url with the URL."
	for _, tc := range []struct {
		tool, args, output string
		images             bool
		want               string
	}{
		{"read_file", `{}`, "   1 | a\n   2 | b\n   3 | c", false, "3 lines"},
		{"read_file", `{}`, "a", false, "1 line"},
		{"read_file", `{}`, "[empty file]", false, "empty file"},
		{"read_file", `{}`, "  40 | x\n  41 | y", false, "lines 40-41"},
		{"read_file", `{}`, "   1 | a\n--- truncated: showing lines 1-2000 of 5120; call read_file again with offset=2001 ---", false, "lines 1-2000 of 5120"},
		{"read_file", `{}`, "   1 | a\n--- truncated: line 1 is longer than 262144 bytes and was cut ---", false, "1 line"},
		{"read_file", `{}`, "app.apk is not a text file: Android APK (ZIP archive), 30.0 MB. read_file shows only text and pictures; list its files with run_command: unzip -l app.apk", false, "Android APK (ZIP archive), 30.0 MB. read_file shows only text and pictures; list its files with run_command: unzip -l app.apk"},
		{"read_file", `{}`, "shots/a.png: PNG image, 1080×2400, 72 KB; shown scaled to 900×2000 as PNG (1.1 MB).", true, "image PNG image, 1080×2400, 72 KB; shown scaled to 900×2000 as PNG (1.1 MB)"},
		{"list_files", `{}`, "a.go\nb.go", false, "2 files"},
		{"list_files", `{}`, "cmd/\ncmd/main.go\ninternal/\ngo.mod", false, "2 files, 2 directories"},
		{"list_files", `{}`, "a.go\n[truncated at 1000 entries]", false, "1 file, cut short"},
		{"list_files", `{}`, "no files found", false, "no files"},
		{"grep", `{"pattern":"count"}`, "a.go\nb.go\nc.go", false, "3 files"},
		{"grep", `{"pattern":"x","output_mode":"count"}`, "a.go:3\nsub/b:c.go:1", false, "4 matches in 2 files"},
		{"grep", `{"pattern":"x","output_mode":"content","context":1}`, "a.go-1-before\na.go:2:x\na.go-3-after\n--\nb.go:9:x\nb.go:10:x", false, "3 matching lines in 2 files"},
		{"grep", `{"pattern":"x","output_mode":"content"}`, "a.go:2:x\n[stopped after 1 lines; there are more matches: narrow the pattern, path or glob, or raise head_limit]", false, "1 matching line in 1 file, more not shown"},
		{"grep", `{"pattern":"x"}`, "no matches found", false, "no matches"},
		{"grep", `{"pattern":"x"}`, "[searched for \"x\" in . (no path given)]\nno matches found", false, "no matches"},
		{"grep", `{"pattern":"x"}`, "[searched for \"x\" in . (no path given)]\na.go\nb.go", false, "2 files"},
		{"fetch_url", `{}`, strings.Repeat("x", 2047) + "\ny", false, "2 KB, 2 lines"},
		{"fetch_url", `{}`, "[redirected to https://example.com/new]\n\nmoved", false, "46 B, 3 lines, redirected to example.com"},
		{"fetch_url", `{}`, "a\nb\n\n... [truncated to 2 lines of 500, 4 KB in all; the full text is saved to /x]", false, "81 B, 4 lines, cut short"},
		{"web_search", `{}`, search, false, "2 results from Brave"},
		{"web_search", `{}`, strings.Replace(search, "):", ", the same search 3 minutes ago):", 1), false, "2 results from Brave (cached)"},
		{"web_search", `{}`, "something unexpected\nmore", false, "something unexpected"},
	} {
		if got, _ := summarizeToolResult(tc.tool, tc.args, tc.output, tc.images); got != tc.want {
			t.Errorf("%s %s %q: got %q, want %q", tc.tool, tc.args, tc.output, got, tc.want)
		}
	}
	_, results := summarizeToolResult("web_search", `{}`, search, false)
	if want := []string{"The Go Programming Language · go.dev", "Go (game) · en.wikipedia.org"}; fmt.Sprint(results) != fmt.Sprint(want) {
		t.Errorf("search results: got %q, want %q", results, want)
	}
}

// TestQuietToolResultIsOneLine checks that a quiet tool prints its call, summary and time on one
// line, web_search's results under it, and an error in full.
func TestQuietToolResultIsOneLine(t *testing.T) {
	out := captureOutput(t, func() {
		printQuietToolResult("grep", `{"pattern":"TODO","glob":"*.go"}`, "a.go\nb.go\n", false, 12*time.Millisecond)
	})
	if want := "\ngrep: TODO (glob: *.go) · 2 files · 12ms\n"; out != want {
		t.Errorf("grep: got %q, want %q", out, want)
	}

	out = captureOutput(t, func() {
		printQuietToolResult("web_search", `{"query":"go"}`, "Search results for \"go\" from Bing (today is 2026-10-23):\n\n1. [Go\x1b]52;c;eA==\x07 docs](https://go.dev/doc/)", false, time.Second)
	})
	if want := "\nweb_search: go · 1 result from Bing · 1.0s\n  │ Go docs · go.dev\n"; out != want {
		t.Errorf("web_search: got %q, want %q", out, want)
	}

	out = captureOutput(t, func() {
		printQuietToolResult("read_file", `{"path":"gone.go"}`, "error: open gone.go: no such file or directory", false, 0)
	})
	if out != "\nread_file: gone.go\n" { // the error itself goes to stderr, as for every tool
		t.Errorf("error: got %q", out)
	}
}

// TestLongCommandShowsItsFirstLines checks that a run_command call of more than 4 lines, such as
// a script in a heredoc, shows its first 3 under the badge with a count of the rest, and that a
// shorter one shows whole.
func TestLongCommandShowsItsFirstLines(t *testing.T) {
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("echo %d", i))
	}
	args := func(n int) string {
		b, _ := json.Marshal(map[string]string{"command": strings.Join(lines[:n], "\n") + "\n"})
		return string(b)
	}
	if got := theme.StripANSI(toolCallLine("run_command", args(20))); !strings.HasSuffix(got, "echo 1\necho 2\necho 3\n… 17 more lines") {
		t.Errorf("20 lines: got %q", got)
	}
	if got := theme.StripANSI(toolCallLine("run_command", args(4))); !strings.HasSuffix(got, "echo 1\necho 2\necho 3\necho 4") {
		t.Errorf("4 lines: got %q", got)
	}

	theme.SetAccessibility(true)
	defer theme.SetAccessibility(false)
	if got := theme.StripANSI(toolCallLine("run_command", args(5))); !strings.HasSuffix(got, "echo 3\n... 2 more lines") {
		t.Errorf("accessible: got %q", got)
	}
}

// TestCommandOutputShowsItsTail checks that run_command's result shows its last 10 lines, the
// exit code among them, under a count of the lines left out, in both views.
func TestCommandOutputShowsItsTail(t *testing.T) {
	var lines []string
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	output := strings.Join(append(lines, "[Exit code 2]"), "\n")

	out := captureOutput(t, func() { printExpandedToolResult("run_command", output, time.Second) })
	if !strings.Contains(out, "  │ … 3 lines hidden\n  │ line 4\n") || !strings.Contains(out, "  │ line 12\n  │ [Exit code 2]\n") || strings.Contains(out, "line 3\n") {
		t.Errorf("got %q", out)
	}

	theme.SetAccessibility(true)
	defer theme.SetAccessibility(false)
	out = captureOutput(t, func() { printExpandedToolResult("run_command", output, time.Second) })
	if !strings.Contains(out, "[RESULT run_command]\n  ... 3 lines hidden\n  line 4\n") || strings.Contains(out, "line 3\n") {
		t.Errorf("accessible: got %q", out)
	}

	short := "ok\n[Exit code 1]"
	if out := captureOutput(t, func() { printExpandedToolResult("run_command", short) }); strings.Contains(out, "hidden") || !strings.Contains(out, "ok") {
		t.Errorf("short output: got %q", out)
	}
}

// outputTool answers every call with fixed text.
type outputTool struct{ name, text string }

func (o outputTool) Name() string                                    { return o.name }
func (o outputTool) Definition() chat.ToolDefinition                 { return chat.ToolDefinition{} }
func (o outputTool) Execute(context.Context, string) (string, error) { return o.text, nil }

// TestQuietToolsPrintOnceInTurnAndHistory checks that a quiet call prints one line, after it
// has run, with no output, and that a resumed conversation shows it the same way.
func TestQuietToolsPrintOnceInTurnAndHistory(t *testing.T) {
	tc := chat.ToolCall{ID: "c1", Function: chat.ToolCallFunction{Name: "list_files", Arguments: `{"path":"src"}`}}
	r := &Runner{Tools: map[string]tools.Tool{"list_files": outputTool{"list_files", "secret.go\nmain.go"}}}
	out := captureOutput(t, func() { r.runTool(context.Background(), tc, nil) })
	if strings.Count(out, "list_files") != 1 || !strings.Contains(out, "list_files: src · 2 files · ") || strings.Contains(out, "secret.go") {
		t.Errorf("turn: got %q", out)
	}

	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "list"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{tc}},
		{Role: "tool", ToolCallID: "c1", Content: "secret.go\nmain.go"},
	}}
	out = captureOutput(t, func() { PrintConversationHistory(conv) })
	if strings.Count(out, "list_files") != 1 || !strings.Contains(out, "list_files: src · 2 files\n") || strings.Contains(out, "secret.go") {
		t.Errorf("history: got %q", out)
	}
}

// TestReplacedResultsAreNotSummarized checks that hmj's note in place of a quiet tool's result,
// for a call cancelled before it ran or a result removed for size, is shown as it is.
func TestReplacedResultsAreNotSummarized(t *testing.T) {
	tc := chat.ToolCall{ID: "c1", Function: chat.ToolCallFunction{Name: "read_file", Arguments: `{"path":"big.log"}`}}
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "assistant", ToolCalls: []chat.ToolCall{tc}},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x\n", minRemovableResultBytes)},
	}}
	if _, _, ok := removeLargestStepResult(conv); !ok {
		t.Fatal("the result was not removed")
	}
	for _, note := range []string{session.CancelledToolResult, conv.Messages[1].Content} {
		out := captureOutput(t, func() { printQuietToolResult("read_file", tc.Function.Arguments, note, false, 0) })
		if want := "\nread_file: big.log · " + note + "\n"; out != want {
			t.Errorf("got %q, want %q", out, want)
		}
	}
}
