package theme

import (
	"strings"
	"testing"
)

// TestColouredToolBadgesAndDiffStats checks the coloured forms: every tool has its own badge
// glyph, an unknown tool gets the generic one, and change stats colour the counts that are
// present.
func TestColouredToolBadgesAndDiffStats(t *testing.T) {
	SetEnabled(true)
	SetAccessibility(false)
	t.Cleanup(func() { SetEnabled(false) })

	badges := map[string]string{
		"web_search":  "🔍 web_search:",
		"fetch_url":   "🌐 fetch_url:",
		"run_command": "⚡ run_command:",
		"other_tool":  "⚙️ other_tool:",
	}
	for name, want := range badges {
		if got := StripANSI(ToolBadge(name)); got != want {
			t.Errorf("ToolBadge(%q) = %q, want %q", name, got, want)
		}
	}

	stats := []struct {
		added, removed int
		want           string
	}{
		{0, 0, "no changes"},
		{3, 0, "+3 lines"},
		{0, 2, "-2 lines"},
		{4, 1, "+4 / -1 lines"},
	}
	for _, tc := range stats {
		got := FormatDiffStats(tc.added, tc.removed)
		if StripANSI(got) != tc.want {
			t.Errorf("FormatDiffStats(%d, %d) = %q, want %q", tc.added, tc.removed, StripANSI(got), tc.want)
		}
		if !strings.Contains(got, "\x1b[") {
			t.Errorf("FormatDiffStats(%d, %d) is not coloured: %q", tc.added, tc.removed, got)
		}
	}
}
