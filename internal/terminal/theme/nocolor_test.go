package theme

import (
	"strings"
	"testing"
)

// With colour off (NO_COLOR, a pipe or --no-color) meaning must survive in text: diff rows keep
// their +/- markers, and status and tool badges keep their words or glyphs.
func TestNoColorKeepsMeaningInText(t *testing.T) {
	defer SetAccessibility(false)
	defer SetEnabled(false)

	t.Setenv("NO_COLOR", "1")
	if ComputeEnabledForTest() {
		t.Fatal("NO_COLOR must disable colour")
	}
	SetEnabled(false)

	for _, accessible := range []bool{false, true} {
		SetAccessibility(accessible)
		var out []string
		out = append(out,
			AddedLine("7", "", "added line"),
			RemovedLine("8", "", "removed line"),
			AddGlyph(), DelGlyph(),
			OK("saved"), Danger("failed"), Caution("careful"),
			ToolBadge("edit_file"), ToolBadge("custom_tool"),
		)
		for _, s := range out {
			if strings.Contains(s, "\x1b") {
				t.Errorf("accessible=%v: escape byte in colour-off output %q", accessible, s)
			}
		}

		if !strings.Contains(out[0], "+") || !strings.Contains(out[0], "added line") {
			t.Errorf("accessible=%v: added row lost its + marker or text: %q", accessible, out[0])
		}
		if !strings.Contains(out[1], "-") || !strings.Contains(out[1], "removed line") {
			t.Errorf("accessible=%v: removed row lost its - marker or text: %q", accessible, out[1])
		}
		if out[2] != "+" || out[3] != "-" {
			t.Errorf("accessible=%v: markers = %q %q, want + and -", accessible, out[2], out[3])
		}
		if accessible {
			assertContainsAll(t, out[4:7], "[OK]", "[ERROR]", "[WARN]")
			if out[7] != "[TOOL edit_file]:" {
				t.Errorf("tool badge = %q", out[7])
			}
		} else {
			assertContainsAll(t, out[4:7], "✔", "✗", "⚠")
			if out[7] != "edit_file:" {
				t.Errorf("tool badge = %q", out[7])
			}
		}
	}
}

func assertContainsAll(t *testing.T, got []string, wants ...string) {
	t.Helper()
	for i, want := range wants {
		if !strings.Contains(got[i], want) {
			t.Errorf("badge %d = %q, want it to contain %q", i, got[i], want)
		}
	}
}

func TestASCIIFallback(t *testing.T) {
	defer SetAccessibility(false)
	const in = "↑/↓ move — …  · → ❯ −"

	SetAccessibility(false)
	if got := ASCII(in); got != in {
		t.Errorf("ASCII changed text outside accessibility mode: %q", got)
	}

	SetAccessibility(true)
	got := ASCII(in)
	for _, r := range got {
		if r >= 0x80 {
			t.Errorf("accessible fallback still contains %q in %q", r, got)
		}
	}
	if !strings.Contains(got, "Up/Down") || !strings.Contains(got, "...") {
		t.Errorf("ASCII(%q) = %q, want arrows and ellipsis spelled out", in, got)
	}
}
