package agent

import (
	"strings"
	"testing"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/tools"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{50 * time.Millisecond, "50ms"},
		{999 * time.Millisecond, "999ms"},
		{1 * time.Second, "1.0s"},
		{1500 * time.Millisecond, "1.5s"},
		{10 * time.Second, "10.0s"},
		{59900 * time.Millisecond, "59.9s"},
		{90 * time.Second, "1m30s"},
		{866 * time.Second, "14m26s"},
		{8400 * time.Second, "2h20m"},
	}
	for _, tc := range cases {
		if got := FormatDuration(tc.d); got != tc.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFormatNumber(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
	}
	for _, tc := range cases {
		if got := FormatNumber(tc.n); got != tc.want {
			t.Errorf("FormatNumber(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestEnsureTrailingNewline(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"hello\n", "hello\n"},
		{"hello", "hello\n"},
		{"a\nb\n", "a\nb\n"},
		{"a\nb", "a\nb\n"},
	}
	for _, tc := range cases {
		if got := EnsureTrailingNewline(tc.in); got != tc.want {
			t.Errorf("EnsureTrailingNewline(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHandleNewKeepsEarlierConversations checks that /new adds a conversation and keeps the
// existing ones, which stay available in /resume.
func TestHandleNewKeepsEarlierConversations(t *testing.T) {
	modelRef := config.ModelRef{Provider: "dummy", Model: "model-a"}
	sm := session.LoadSessionManager("", modelRef)
	if len(sm.List()) != 1 {
		t.Fatalf("expected 1 conversation at start, got %d", len(sm.List()))
	}

	if err := HandleNew(sm); err != nil {
		t.Fatalf("HandleNew failed: %v", err)
	}
	if len(sm.List()) != 2 {
		t.Fatalf("expected 2 conversations after new, got %d", len(sm.List()))
	}
}

func TestNewRunnerWithTools(t *testing.T) {
	cfg := config.Config{}
	client := chat.NewClient(cfg)
	runner := NewRunnerWithTools(client, cfg, []tools.Tool{&tools.ReadFileTool{}})

	if len(runner.Tools) != 1 {
		t.Fatalf("expected 1 tool registered, got %d", len(runner.Tools))
	}
	if _, ok := runner.Tools["read_file"]; !ok {
		t.Errorf("expected read_file tool in runner.Tools")
	}
	if len(runner.ToolDefs) != 1 || runner.ToolDefs[0].Function.Name != "read_file" {
		t.Errorf("expected read_file in ToolDefs: %+v", runner.ToolDefs)
	}
}

func TestPrintExpandedToolResult(t *testing.T) {
	for _, tc := range []struct {
		tool, output string
		d            time.Duration
		want         []string
	}{
		{"read_file", "[empty file]", 0, []string{"[empty file]"}},
		{"read_file", "line 1\nline 2", 150 * time.Millisecond, []string{"line 1", "line 2", "150ms"}},
		{"write_file", "error: something went wrong", 50 * time.Millisecond, []string{"50ms"}},
		{"generic_tool", "output row 1\noutput row 2", 0, []string{"output row 1", "output row 2"}},
	} {
		out := captureOutput(t, func() { printExpandedToolResult(tc.tool, tc.output, tc.d) })
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s %q: output %q lacks %q", tc.tool, tc.output, out, w)
			}
		}
	}
	if errOut := captureStderr(t, func() { printExpandedToolResult("write_file", "error: something went wrong") }); !strings.Contains(errOut, "something went wrong") {
		t.Errorf("an error result is not on stderr: %q", errOut)
	}
	if out := captureOutput(t, func() { printExpandedToolResult("write_file", "") }); strings.Contains(out, "\n\n\n") {
		t.Errorf("an empty result printed blank lines: %q", out)
	}
}
