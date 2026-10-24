package theme

import (
	"os"
	"strings"
	"testing"
)

func TestSanitizeDropsControlsButKeepsTextAndLayout(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "hello world", "hello world"},
		{"tabs and newlines survive", "a\tb\nc", "a\tb\nc"},
		{"carriage return removed", "old\rnew", "oldnew"},
		{"CRLF keeps the line feed", "one\r\ntwo", "one\ntwo"},
		{"BEL and NUL dropped", "a\x07b\x00c", "abc"},
		{"DEL dropped", "a\x7fb", "ab"},
		{"C1 control rune dropped", "a\u0085b\u009bc", "abc"},
		{"CSI colour removed before control scan", "\x1b[31mred\x1b[0m", "red"},
		{"OSC title removed with BEL terminator", "\x1b]0;title\x07body", "body"},
		{"OSC removed with ST terminator", "\x1b]52;c;aGk=\x1b\\after", "after"},
		{"two-char ESC sequence removed", "x\x1bMy", "xy"},
		{"unicode text preserved", "héllo 日本 🙂", "héllo 日本 🙂"},
		{"lone trailing ESC is dropped", "end\x1b", "end"},
		{"DCS query removed with its payload", "a\x1bP$qm\x1b\\b", "ab"},
		{"APC graphics command removed with its payload", "a\x1b_Gf=24;AAAA\x1b\\b", "ab"},
		{"PM and SOS removed with their payload", "a\x1b^hidden\x1b\\b\x1bXmore\x1b\\c", "abc"},
		{"BEL does not end a DCS", "a\x1bP1\x07still\x1b\\b", "ab"},
		{"right-to-left override shown", "if x \u202e} \u2066else\u2069 {", "if x <U+202E>} <U+2066>else<U+2069> {"},
		{"Arabic and Hebrew text untouched", "مرحبا שלום", "مرحبا שלום"},
		{"left-to-right mark untouched", "a\u200eb", "a\u200eb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sanitize(tc.in); got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripANSIHandlesUnterminatedSequences(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"no escapes", "abc", "abc"},
		{"CSI with params", "\x1b[1;38;5;208mX\x1b[m", "X"},
		{"unterminated CSI runs to end", "a\x1b[31", "a"},
		{"unterminated OSC runs to end", "a\x1b]0;never ends", "a"},
		{"OSC 8 hyperlink with ST", "\x1b]8;;http://x\x1b\\link", "link"},
		{"ESC at very end is kept", "a\x1b", "a\x1b"},
		{"two-char ESC sequence", "a\x1bcb", "ab"},
		{"unterminated DCS runs to end", "a\x1bPq#0;2;0;0;0", "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripANSI(tc.in); got != tc.want {
				t.Errorf("StripANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSGRHelpersWhenEnabledAndDisabled(t *testing.T) {
	SetEnabled(true)
	t.Cleanup(func() { SetEnabled(false) })

	if got := Raw("1", "31"); got != "\x1b[1;31m" {
		t.Errorf("Raw enabled = %q", got)
	}
	if got := Raw(); got != "" {
		t.Errorf("Raw with no codes = %q, want empty", got)
	}
	if got := Reset(); got != "\x1b["+resetParam+"m" {
		t.Errorf("Reset enabled = %q", got)
	}
	if got := sgr(); got != "" {
		t.Errorf("sgr() = %q, want empty", got)
	}
	if got := sgr("4"); got != "\x1b[4m" {
		t.Errorf("sgr(4) = %q", got)
	}
	if got := Style("", "1"); got != "" {
		t.Errorf("Style of empty string = %q, want empty", got)
	}
	if got := Style("x"); got != "x" {
		t.Errorf("Style with no codes = %q, want x", got)
	}

	SetEnabled(false)
	if Raw("1") != "" || Reset() != "" {
		t.Error("Raw/Reset must be empty while disabled")
	}
}

func TestColorWantedFollowsTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env      map[string]string
		terminal bool
		want     bool
	}{
		{nil, true, true},
		{nil, false, false},
		{map[string]string{"NO_COLOR": "1"}, true, false},
		{map[string]string{"NO_COLOR": "0"}, true, false},
		{map[string]string{"NO_COLOR": "false"}, true, false},
		{map[string]string{"NO_COLOR": "1", "FORCE_COLOR": "1", "CLICOLOR_FORCE": "1"}, true, false},
		{map[string]string{"FORCE_COLOR": "1"}, false, true},
		{map[string]string{"FORCE_COLOR": "0"}, false, true},
		{map[string]string{"CLICOLOR_FORCE": "1"}, false, true},
		{map[string]string{"CLICOLOR_FORCE": "0"}, false, false},
		{map[string]string{"FORCE_COLOR": "1", "TERM": "dumb", "CLICOLOR": "0"}, false, true},
		{map[string]string{"CLICOLOR": "0"}, true, false},
		{map[string]string{"CLICOLOR": "1"}, true, true},
		{map[string]string{"TERM": "DUMB"}, true, false},
	} {
		if got := colorWanted(func(k string) string { return tc.env[k] }, tc.terminal); got != tc.want {
			t.Errorf("colorWanted(%v, terminal %t) = %t, want %t", tc.env, tc.terminal, got, tc.want)
		}
	}
}

func TestIsTerminalFalseForPipeAndRegularFile(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(r.Fd()) {
		t.Error("pipe reported as terminal")
	}
}

func TestToolBadgeAccessibleAndPlain(t *testing.T) {
	SetEnabled(false)
	t.Cleanup(func() { SetAccessibility(false) })
	names := []string{"read_file", "write_file", "run_command", "unknown_tool"}

	SetAccessibility(false)
	for _, n := range names {
		if got := ToolBadge(n); got == "" || !strings.Contains(got, n) {
			t.Errorf("plain ToolBadge(%q) = %q", n, got)
		}
	}
	SetAccessibility(true)
	for _, n := range names {
		want := "[TOOL " + n + "]:"
		if got := ToolBadge(n); got != want {
			t.Errorf("accessible ToolBadge(%q) = %q, want %q", n, got, want)
		}
	}
	SetEnabled(true)
	for _, n := range names {
		want := "[TOOL " + n + "]:"
		if got := ToolBadge(n); !strings.Contains(got, want) {
			t.Errorf("coloured accessible ToolBadge(%q) = %q, want it to contain %q", n, got, want)
		}
	}
}

func TestFormatDiffStatsAccessibleForms(t *testing.T) {
	SetEnabled(false)
	SetAccessibility(true)
	t.Cleanup(func() { SetAccessibility(false) })
	tests := []struct {
		added, removed int
		want           string
	}{
		{0, 0, "[no changes]"},
		{3, 0, "[+3 lines]"},
		{0, 2, "[-2 lines]"},
		{4, 1, "[+4 / -1 lines]"},
	}
	for _, tc := range tests {
		if got := FormatDiffStats(tc.added, tc.removed); got != tc.want {
			t.Errorf("FormatDiffStats(%d,%d) accessible = %q, want %q", tc.added, tc.removed, got, tc.want)
		}
	}
}

func TestFormatDiffStatsPlainForms(t *testing.T) {
	SetEnabled(false)
	tests := []struct {
		added, removed int
		want           string
	}{
		{0, 0, "no changes"},
		{5, 0, "+5 lines"},
		{0, 7, "-7 lines"},
		{2, 3, "+2 / -3 lines"},
	}
	for _, tc := range tests {
		if got := FormatDiffStats(tc.added, tc.removed); got != tc.want {
			t.Errorf("FormatDiffStats(%d,%d) = %q, want %q", tc.added, tc.removed, got, tc.want)
		}
	}
}

func TestSetAccessibilityOnlyTogglesAccessibility(t *testing.T) {
	t.Cleanup(func() { SetAccessibility(false) })
	SetAccessibility(true)
	if !Accessibility() {
		t.Fatal("Accessibility() = false after SetAccessibility(true)")
	}
	if got := ASCII("wait… → done"); got != "wait... -> done" {
		t.Errorf("ASCII in accessible mode = %q, want %q", got, "wait... -> done")
	}
	SetAccessibility(false)
	if Accessibility() {
		t.Error("Accessibility() still true after reset")
	}
	if got := ASCII("wait…"); got != "wait…" {
		t.Errorf("ASCII outside accessibility mode changed text: %q", got)
	}
}
