package theme

import (
	"strings"
	"testing"
)

func TestThemeDisabledInTests(t *testing.T) {
	// Tests run with stdout as a pipe, so colour must be off by default: every
	// style helper has to return its input unchanged.
	SetEnabled(false)
	defer SetEnabled(false)

	inputs := []func(string) string{
		Title, Muted, Faint, Prompt, User,
		Accent, FilePath, Added, Removed,
		Info, Warn, Bold, Mark,
	}
	for i, fn := range inputs {
		if got := fn("x"); got != "x" {
			t.Fatalf("style #%d emitted escapes while disabled: %q", i, got)
		}
	}
	if Raw("1") != "" || Reset() != "" {
		t.Fatal("Raw/Reset must be empty while disabled")
	}
}

func TestThemeEnabledEmitsCodes(t *testing.T) {
	SetEnabled(true)
	defer SetEnabled(false)

	if got := Danger("boom"); got != "\x1b[38;5;210m✗ boom\x1b[0m" {
		t.Fatalf("Danger() = %q, want coral red", got)
	}
	if got := AddGlyph(); got != "\x1b[1;38;5;114m+\x1b[0m" {
		t.Fatalf("AddGlyph() = %q, want green +", got)
	}
	if got := DelGlyph(); got != "\x1b[1;38;5;210m-\x1b[0m" {
		t.Fatalf("DelGlyph() = %q, want coral red -", got)
	}
	if got := Style("", "1;31"); got != "" {
		t.Fatalf("empty input should stay empty, got %q", got)
	}
	if !strings.Contains(OK("done"), "✔") {
		t.Fatalf("OK should carry a check glyph, got %q", OK("done"))
	}
	if !strings.HasPrefix(OK("done"), "\x1b[38;5;114m") {
		t.Fatalf("OK should be spring green, got %q", OK("done"))
	}
}

func TestThemeStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"a\x1b[1;32mb\x1b[0mc", "abc"},
		{"\x1b[2Jcleared", "cleared"},
		{"\x1b[10;20Hmoved", "moved"},
		{"title\x1b]0;window\x07next", "titlenext"},
		{"title\x1b]8;;http://x\x1b\\link", "titlelink"},
		{"\x1bMtwo-char", "two-char"},
		{"truncated\x1b[31", "truncated"}, // unterminated CSI dropped safely
	}
	for _, tc := range cases {
		if got := StripANSI(tc.in); got != tc.want {
			t.Errorf("StripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestThemeSanitize(t *testing.T) {
	// Escape sequences and stray carriage returns must not survive; tabs and
	// newlines must.
	in := "\x1b[31merror\x1b[0m\r\nline2\tcol\rCR"
	got := Sanitize(in)
	if strings.ContainsAny(got, "\r\x1b") {
		t.Fatalf("sanitize kept control chars: %q", got)
	}
	if !strings.Contains(got, "error") || !strings.Contains(got, "\n") || !strings.Contains(got, "\t") {
		t.Fatalf("sanitize dropped legitimate content: %q", got)
	}
	// C1 range (0x80-0x9f bytes as runes) also stripped.
	if s := Sanitize("a\u009bb"); s != "ab" {
		t.Fatalf("C1 control not stripped: %q", s)
	}
}

func TestThemeCountersAndRules(t *testing.T) {
	SetEnabled(false)
	defer SetEnabled(false)

	if Rule(3) != "───" {
		t.Fatalf("Rule(3) = %q", Rule(3))
	}
}

func TestThemeTerminalDetection(t *testing.T) {
	if IsTerminal(^uintptr(0)) {
		t.Fatal("bogus fd must not report as terminal")
	}
}

func TestThemeToolBadges(t *testing.T) {
	SetEnabled(false)
	defer SetEnabled(false)

	tools := []string{"read_file", "write_file", "edit_file", "list_files", "run_command", "web_search", "fetch_url"}
	for _, name := range tools {
		if got := ToolBadge(name); got != name+":" {
			t.Errorf("ToolBadge(%q) disabled = %q, want %q", name, got, name+":")
		}
	}

	SetEnabled(true)
	if !strings.Contains(ToolBadge("read_file"), "📖") {
		t.Errorf("expected 📖 icon in read_file badge, got %q", ToolBadge("read_file"))
	}
	if !strings.Contains(ToolBadge("edit_file"), "✏️") {
		t.Errorf("expected ✏️ icon in edit_file badge, got %q", ToolBadge("edit_file"))
	}
	if !strings.Contains(ToolBadge("run_command"), "⚡") {
		t.Errorf("expected ⚡ icon in run_command badge, got %q", ToolBadge("run_command"))
	}
	if !strings.Contains(ToolBadge("list_files"), "📁") {
		t.Errorf("expected 📁 icon in list_files badge, got %q", ToolBadge("list_files"))
	}
}

func TestThemeFormatDiffStats(t *testing.T) {
	SetEnabled(false)
	defer SetEnabled(false)

	if got := FormatDiffStats(5, 5); got != "+5 / -5 lines" {
		t.Fatalf("disabled FormatDiffStats(5, 5) = %q, want '+5 / -5 lines'", got)
	}
	if got := FormatDiffStats(3, 0); got != "+3 lines" {
		t.Fatalf("disabled FormatDiffStats(3, 0) = %q, want '+3 lines'", got)
	}
	if got := FormatDiffStats(0, 2); got != "-2 lines" {
		t.Fatalf("disabled FormatDiffStats(0, 2) = %q, want '-2 lines'", got)
	}
	if got := FormatDiffStats(0, 0); got != "no changes" {
		t.Fatalf("disabled FormatDiffStats(0, 0) = %q, want 'no changes'", got)
	}

	SetEnabled(true)
	gotColored := FormatDiffStats(5, 5)
	if !strings.Contains(gotColored, "\x1b[38;5;114m+5\x1b[0m") {
		t.Fatalf("enabled FormatDiffStats(5, 5) should contain green +5, got %q", gotColored)
	}
	if !strings.Contains(gotColored, "\x1b[38;5;210m-5\x1b[0m") {
		t.Fatalf("enabled FormatDiffStats(5, 5) should contain red -5, got %q", gotColored)
	}
	if !strings.Contains(gotColored, "lines") {
		t.Fatalf("enabled FormatDiffStats(5, 5) should contain 'lines', got %q", gotColored)
	}
}

func TestThemeSPngHelpers(t *testing.T) {
	SetEnabled(false)
	defer SetEnabled(false)

	if got := Danger("err"); got != "✗ err" {
		t.Fatalf("disabled Danger = %q", got)
	}
	if got := Caution("warn"); got != "⚠ warn" {
		t.Fatalf("disabled Caution = %q", got)
	}
	if got := RemovedLine(" 5 ", "-", "func main() {"); got != " 5 - func main() {" {
		t.Fatalf("disabled RemovedLine = %q", got)
	}
	if got := AddedLine(" 5 ", "+", "func greet() {"); got != " 5 + func greet() {" {
		t.Fatalf("disabled AddedLine = %q", got)
	}
	// Indentation must be preserved in disabled mode
	if got := RemovedLine(" 5 ", "-", "    x := 1"); got != " 5 -     x := 1" {
		t.Fatalf("disabled RemovedLine lost indentation: %q", got)
	}
	if got := AddedLine(" 5 ", "+", "\t\ty := 2"); got != " 5 + \t\ty := 2" {
		t.Fatalf("disabled AddedLine lost indentation: %q", got)
	}

	SetEnabled(true)
	rem := RemovedLine(" 5 ", "-", "func main() {")
	if !strings.Contains(rem, "48;5;52") || !strings.Contains(rem, "38;5;210") {
		t.Fatalf("enabled RemovedLine missing dark red bg or coral text: %q", rem)
	}

	add := AddedLine(" 5 ", "+", "func greet() {")
	if !strings.Contains(add, "48;5;234") || !strings.Contains(add, "38;5;114") {
		t.Fatalf("enabled AddedLine missing dark charcoal bg or spring green text: %q", add)
	}

	// Indentation and embedded inline marks must be preserved in enabled mode
	remIndented := RemovedLine(" 5 ", "-", "    x := 1")
	if !strings.Contains(remIndented, "    x := 1") {
		t.Fatalf("enabled RemovedLine lost indentation: %q", remIndented)
	}
	addIndented := AddedLine(" 5 ", "+", "\t\ty := 2")
	if !strings.Contains(addIndented, "\t\ty := 2") {
		t.Fatalf("enabled AddedLine lost indentation: %q", addIndented)
	}

	// Embedded mark must restore line background and not insert stray spaces
	marked := "const Version = \"" + Mark("v0.1.0") + "\""
	remMarked := RemovedLine(" 1 ", "-", marked)
	if strings.Contains(remMarked, "\"  v0.1.0  \"") {
		t.Fatalf("stray spaces found around token: %q", remMarked)
	}
	if !strings.Contains(remMarked, "v0.1.0") {
		t.Fatalf("marked text missing: %q", remMarked)
	}
	if !strings.Contains(remMarked, "\x1b[48;5;52;38;5;210m\"") {
		t.Fatalf("expected background and text restoration after mark reset: %q", remMarked)
	}
}
