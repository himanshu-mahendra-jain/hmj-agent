package terminal

import (
	"strings"
	"testing"

	"hmj-agent/internal/terminal/theme"
)

func TestComprehensiveThemeAccessibility(t *testing.T) {
	theme.SetAccessibility(true)
	defer theme.SetAccessibility(false)

	if !theme.Accessibility() {
		t.Fatal("expected Accessibility to be true")
	}

	if !strings.Contains(theme.OK("success"), "[OK]") {
		t.Errorf("expected [OK], got %s", theme.OK("success"))
	}
	if !strings.Contains(theme.Note("info"), "[NOTE]") {
		t.Errorf("expected [NOTE], got %s", theme.Note("info"))
	}
	if !strings.Contains(theme.Danger("fail"), "[ERROR]") {
		t.Errorf("expected [ERROR], got %s", theme.Danger("fail"))
	}
	if !strings.Contains(theme.Caution("warn"), "[WARN]") {
		t.Errorf("expected [WARN], got %s", theme.Caution("warn"))
	}
	if !strings.Contains(theme.Hint("tip"), "[HINT]") {
		t.Errorf("expected [HINT], got %s", theme.Hint("tip"))
	}
}

func TestThemeThinking(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)
	if got := theme.Thinking(); got == "" {
		t.Error("Thinking() returned empty string while enabled")
	}
	theme.SetEnabled(false)
	if got := theme.Thinking(); got != "" {
		t.Error("Thinking() must be empty while disabled")
	}
}

func TestAccessibilityModeFormatting(t *testing.T) {
	theme.SetAccessibility(true)
	defer theme.SetAccessibility(false)

	// 1. Tool badges should avoid emoji and output clean text prefix
	badge := theme.ToolBadge("read_file")
	if !strings.Contains(badge, "[TOOL read_file]:") {
		t.Errorf("expected accessibility ToolBadge to contain '[TOOL read_file]:', got %q", badge)
	}

	// 2. FormatDiffStats should output brackets
	stats := theme.FormatDiffStats(12, 4)
	if stats != "[+12 / -4 lines]" {
		t.Errorf("expected '[+12 / -4 lines]', got %q", stats)
	}

	// 3. Rule should use ASCII hyphens
	rule := theme.Rule(5)
	if !strings.Contains(rule, "-----") {
		t.Errorf("expected ASCII hyphens in Rule, got %q", rule)
	}

	// 4. DiffHeader should use pipe separator
	header := theme.DiffHeader("file.go", "[+1 lines]")
	if !strings.Contains(header, "|") {
		t.Errorf("expected pipe separator in DiffHeader, got %q", header)
	}

	// 5. Table should render ASCII borders
	lines := []string{
		"| Col 1 | Col 2 |",
		"| ----- | ----- |",
		"| Val 1 | Val 2 |",
	}
	tbl, ok := ParseMarkdownTable(lines)
	if !ok {
		t.Fatal("failed to parse table")
	}
	rendered := tbl.Render()
	if strings.Contains(rendered, "┌") || strings.Contains(rendered, "│") {
		t.Errorf("expected ASCII table borders in accessibility mode, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "+") || !strings.Contains(rendered, "|") {
		t.Errorf("expected + and | borders in ASCII table, got:\n%s", rendered)
	}
}

func TestStripANSI(t *testing.T) {
	// stripANSI is unexported; test via ReadSeamlessInput paste path
	// We test indirectly via CleanInput which strips bracketed paste markers
	// and the paste path strips ANSI before inserting.
	// Test that text with ANSI codes is cleaned by CleanInput helper.
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "colour codes stripped",
			input: "\x1b[32mhello\x1b[0m world",
			want:  "hello world",
		},
		{
			name:  "cursor movement stripped",
			input: "abc\x1b[2Adef",
			want:  "abcdef",
		},
		{
			name:  "plain text unchanged",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "multiline plain text unchanged",
			input: "line1\nline2\nline3",
			want:  "line1\nline2\nline3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := theme.StripANSI(tc.input)
			if got != tc.want {
				t.Fatalf("StripANSI(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
