package theme

import (
	"strings"
	"testing"
)

func TestHighContrastPrecedence(t *testing.T) {
	defer SetHighContrast(false)
	defer SetEnabled(false)
	SetEnabled(true)

	const defaultMuted = "\x1b[38;5;109mm\x1b[0m"
	const brightMuted = "\x1b[97mm\x1b[0m"

	tests := []struct {
		name     string
		env      string
		flag     bool // SetHighContrast, as --high-contrast or config does
		wantOn   bool
		wantMute string
	}{
		{"default", "", false, false, defaultMuted},
		{"flag", "", true, true, brightMuted},
		{"env 1", "1", false, true, brightMuted},
		{"env true", "true", false, true, brightMuted},
		{"env 0 does not enable", "0", false, false, defaultMuted},
		{"env cannot be turned off by a reload", "1", false, true, brightMuted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HIGH_CONTRAST", tt.env)
			SetHighContrast(tt.flag)
			if got := HighContrast(); got != tt.wantOn {
				t.Errorf("HighContrast() = %v, want %v", got, tt.wantOn)
			}
			if got := Muted("m"); got != tt.wantMute {
				t.Errorf("Muted() = %q, want %q", got, tt.wantMute)
			}
		})
	}
}

func TestHighContrastPaletteIsBrightAndNotDim(t *testing.T) {
	p := HighContrastPalette()
	for name, codes := range map[string][]string{
		"Faint": p.Faint, "Muted": p.Muted, "Text": p.Text, "Error": p.Error,
		"Added": p.Added, "Removed": p.Removed, "MarkAdd": p.MarkAdd, "MarkDel": p.MarkDel,
	} {
		for _, c := range codes {
			if c == paramDim {
				t.Errorf("%s uses dim intensity (SGR 2)", name)
			}
		}
	}
	if !strings.Contains(strings.Join(p.Error, ";"), paramBold) {
		t.Errorf("errors must be bold, got %v", p.Error)
	}
	if !strings.Contains(strings.Join(p.Removed, ";"), paramUnderline) {
		t.Errorf("removed text must be underlined, got %v", p.Removed)
	}
}

func TestHighContrastDiffRowsCarryMarkers(t *testing.T) {
	defer SetHighContrast(false)
	defer SetEnabled(false)
	SetEnabled(true)
	SetHighContrast(true)

	added := AddedLine("4", "+", "new text")
	removed := RemovedLine("5", "-", "old text")
	if !strings.Contains(added, "+") || !strings.Contains(removed, "-") {
		t.Fatalf("rows lost their markers: %q / %q", added, removed)
	}
	if !strings.Contains(removed, "\x1b[0m") || !strings.Contains(removed, ";4m") {
		t.Errorf("removed row should be underlined in high contrast: %q", removed)
	}
	if strings.Contains(added, "48;5;") || strings.Contains(removed, "48;5;") {
		t.Errorf("high-contrast rows should not use background fills: %q", added)
	}
}
