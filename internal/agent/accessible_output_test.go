package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/terminal/theme"
	"hmj-agent/v2/internal/tools"
)

// captureOutput returns what fn writes to os.Stdout.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	os.Stdout = old
	w.Close()
	return <-out
}

// escapeSeq matches one CSI sequence (ESC [ params final-byte).
var escapeSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// assertAppendOnly fails when s holds anything but colour (SGR) escapes, or when it has
// non-ASCII text the accessible mode must not print.
func assertAppendOnly(t *testing.T, s string) {
	t.Helper()
	for _, esc := range escapeSeq.FindAllString(s, -1) {
		if !strings.HasSuffix(esc, "m") {
			t.Errorf("accessible output has cursor or erase sequence %q", esc)
		}
	}
	for _, r := range s {
		if r >= 0x80 {
			t.Errorf("accessible output has non-ASCII rune %q in %q", r, s)
			return
		}
	}
}

func TestResultBlockFormat(t *testing.T) {
	tests := []struct {
		name, tool, output, want string
	}{
		{"named tool", "read_file", "a\nb", "[RESULT read_file]\n  a\n  b\n[END RESULT]"},
		{"no name", "", "only", "[OUTPUT]\n  only\n[END RESULT]"},
		{"single empty line", "run_command", "", "[RESULT run_command]\n  \n[END RESULT]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resultBlock(tt.tool, tt.output); got != tt.want {
				t.Errorf("resultBlock(%q, %q) = %q, want %q", tt.tool, tt.output, got, tt.want)
			}
		})
	}
}

func TestToolOutputUsesBlocksInAccessibleMode(t *testing.T) {
	theme.SetAccessibility(true)
	theme.SetEnabled(true)
	defer theme.SetAccessibility(false)
	defer theme.SetEnabled(false)

	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "list"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "t1", Function: chat.ToolCallFunction{Name: "run_command", Arguments: `{"command":"ls"}`}}}},
		{Role: "tool", ToolCallID: "t1", Content: "a.go\nb.go"},
		{Role: "assistant", Content: "done"},
	}}

	out := captureOutput(t, func() { PrintConversationHistory(conv) })
	if !strings.Contains(out, "[RESULT run_command]\n  a.go\n  b.go\n[END RESULT]") {
		t.Errorf("expected a framed result block, got %q", out)
	}
	if strings.Contains(out, "│") {
		t.Errorf("accessible output kept the gutter glyph: %q", out)
	}
	assertAppendOnly(t, strings.ReplaceAll(out, "│", "|"))
}

func TestTurnHistoryViewUsesBlocksInAccessibleMode(t *testing.T) {
	theme.SetAccessibility(true)
	defer theme.SetAccessibility(false)

	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "t1", Function: chat.ToolCallFunction{Name: "read_file"}}}},
		{Role: "tool", ToolCallID: "t1", Content: "line"},
	}}
	turns := turnsOf(conv.Messages)
	if len(turns) != 1 || !strings.Contains(turns[0].Response, "[RESULT read_file]\n  line\n[END RESULT]") {
		t.Fatalf("turn response = %q, want a framed block", turns[0].Response)
	}
}

func TestAccessibleShortcutsAreASCII(t *testing.T) {
	theme.SetAccessibility(true)
	theme.SetEnabled(false)
	defer theme.SetAccessibility(false)

	assertAppendOnly(t, ShortcutsTable().Render())
}

// Accessibility mode lists only the keys that work there: the Ctrl+ editing, history and
// key-driven shortcuts are gone, slash commands and the halt and exit keys stay, and the
// multi-line rule is noted. The default table keeps every row.
func TestAccessibleShortcutsOmitBrokenKeys(t *testing.T) {
	theme.SetEnabled(false)
	defer theme.SetAccessibility(false)

	theme.SetAccessibility(true)
	accessible := ShortcutsTable().Render()
	for _, gone := range []string{"Ctrl+N", "Ctrl+R", "Ctrl+O", "Ctrl+Y", "Ctrl+L", "Ctrl+A", "Ctrl+E", "Ctrl+U", "Ctrl+K", "Ctrl+W", "Up", "Down", "Ctrl+E rename", "/clear", "/compact"} {
		if strings.Contains(accessible, gone) {
			t.Errorf("accessible table still lists %q:\n%s", gone, accessible)
		}
	}
	for _, want := range []string{"/new", "/resume", "/models", "/history", "/health", "/shortcuts", "Esc", "Ctrl+C", "Ctrl+D", "End a line with"} {
		if !strings.Contains(accessible, want) {
			t.Errorf("accessible table lost %q:\n%s", want, accessible)
		}
	}
	if !strings.Contains(accessible, "`\\`") && !strings.Contains(accessible, "\\ to continue") {
		t.Errorf("expected the trailing-backslash note in:\n%s", accessible)
	}

	theme.SetAccessibility(false)
	def := ShortcutsTable().Render()
	for _, want := range []string{"Ctrl+N", "Ctrl+R", "↑", "↓", "Ctrl+E rename"} {
		if !strings.Contains(def, want) {
			t.Errorf("default table lost %q", want)
		}
	}
	// The standard shell editing keys work but are not listed, and /compact is gone.
	for _, gone := range []string{"Ctrl+A", "Ctrl+U", "Ctrl+K", "Ctrl+W", "/compact"} {
		if strings.Contains(def, gone) {
			t.Errorf("default table still lists %q", gone)
		}
	}
}

func TestRemediationHintPrintsOnlyInAccessibleMode(t *testing.T) {
	err := &chat.StatusError{Code: 404, Body: "missing"}

	theme.SetAccessibility(false)
	var plain strings.Builder
	PrintHint(&plain, err)
	if plain.Len() != 0 {
		t.Errorf("hint printed outside accessibility mode: %q", plain.String())
	}

	theme.SetAccessibility(true)
	defer theme.SetAccessibility(false)
	var labelled strings.Builder
	PrintHint(&labelled, err)
	if want := "[HINT] check the model id and baseUrl\n"; labelled.String() != want {
		t.Errorf("PrintHint = %q, want %q", labelled.String(), want)
	}
}

// File content is shown as-is in accessibility mode; only the renderer's own notices get
// the ASCII fallback.
func TestAccessibleDiffKeepsFileContent(t *testing.T) {
	theme.SetAccessibility(true)
	theme.SetEnabled(false)
	defer theme.SetAccessibility(false)

	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("keep → arrow\nold\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureOutput(t, func() { printEditFileDiff(path, "old", "new") })
	if !strings.Contains(out, "keep → arrow") {
		t.Errorf("context line content was altered: %q", out)
	}
}

// fakeTool runs for d, or until its context ends, and then returns "done". panicAfter
// makes it panic instead of returning.
type fakeTool struct {
	d          time.Duration
	panicAfter bool
}

func (fakeTool) Name() string                    { return "slow_tool" }
func (fakeTool) Definition() chat.ToolDefinition { return chat.ToolDefinition{} }

func (f fakeTool) Execute(ctx context.Context, _ string) (string, error) {
	select {
	case <-time.After(f.d):
	case <-ctx.Done():
	}
	if f.panicAfter {
		panic("tool exploded")
	}
	return "done", nil
}

// runFakeTool runs one tool call through runTool and returns the stdout it produced.
// testProgressEvery is the progress interval runFakeTool's runner reports at.
var testProgressEvery time.Duration

func runFakeTool(t *testing.T, ctx context.Context, tool tools.Tool) string {
	t.Helper()
	r := &Runner{Tools: map[string]tools.Tool{tool.Name(): tool}, progressEvery: testProgressEvery}
	tc := chat.ToolCall{ID: "c1", Function: chat.ToolCallFunction{Name: tool.Name(), Arguments: "{}"}}
	return captureOutput(t, func() { r.runTool(ctx, tc, nil) })
}

// progressLinesStopBeforeResult checks that every progress line comes before the duration
// line, which runTool prints last on every path, so nothing is written after runTool returns.
func progressLinesStopBeforeResult(t *testing.T, out string) int {
	t.Helper()
	finished := strings.LastIndex(out, "\n  (")
	if finished < 0 {
		t.Fatalf("no duration line in output %q", out)
	}
	if last := strings.LastIndex(out, "[running] "); last > finished {
		t.Errorf("progress line printed after the tool finished: %q", out)
	}
	return strings.Count(out, "[running] slow_tool (")
}

func TestToolProgressInAccessibleMode(t *testing.T) {
	theme.SetAccessibility(true)
	theme.SetEnabled(false)
	testProgressEvery = 10 * time.Millisecond
	defer func() {
		theme.SetAccessibility(false)
		testProgressEvery = 0
	}()

	t.Run("long tool prints periodic lines then stops", func(t *testing.T) {
		out := runFakeTool(t, context.Background(), fakeTool{d: 60 * time.Millisecond})
		if n := progressLinesStopBeforeResult(t, out); n < 2 {
			t.Errorf("expected periodic [running] lines, got %d in %q", n, out)
		}
	})

	t.Run("halted tool stops its ticker", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			time.Sleep(35 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		out := runFakeTool(t, ctx, fakeTool{d: 5 * time.Second})
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("cancelled tool did not return promptly: %v", elapsed)
		}
		progressLinesStopBeforeResult(t, out)
	})

	t.Run("panicking tool still stops its ticker", func(t *testing.T) {
		out := runFakeTool(t, context.Background(), fakeTool{d: 45 * time.Millisecond, panicAfter: true})
		progressLinesStopBeforeResult(t, out)
	})

	t.Run("tool finishing within one interval prints nothing", func(t *testing.T) {
		testProgressEvery = time.Second
		defer func() { testProgressEvery = 10 * time.Millisecond }()
		out := runFakeTool(t, context.Background(), fakeTool{d: time.Millisecond})
		if strings.Contains(out, "[running]") {
			t.Errorf("fast tool printed progress: %q", out)
		}
	})
}

func TestToolProgressOffInDefaultMode(t *testing.T) {
	theme.SetAccessibility(false)
	testProgressEvery = 10 * time.Millisecond
	defer func() { testProgressEvery = 0 }()

	out := runFakeTool(t, context.Background(), fakeTool{d: 60 * time.Millisecond})
	if strings.Contains(out, "[running]") {
		t.Errorf("default mode must not print progress lines: %q", out)
	}
}
