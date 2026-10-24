package theme

import (
	"os"
	"strings"
)

var highContrastMode bool

// SetHighContrast forces high-contrast mode on or off and swaps the palette to match.
// Exposed for the --high-contrast flag, config and tests. HIGH_CONTRAST=1 keeps the
// bright palette active regardless of this setting.
func SetHighContrast(on bool) {
	highContrastMode = on
	pal = selectPalette()
}

// HighContrast reports whether the bright, bold high-contrast palette is active.
func HighContrast() bool {
	if highContrastMode {
		return true
	}
	v := os.Getenv("HIGH_CONTRAST")
	return v == "1" || strings.EqualFold(v, "true")
}

// HighContrastPalette is the bright, bold scheme used by high-contrast mode. It uses
// bright foregrounds only (Faint and Muted are normal-intensity white), no background
// fills, and never relies on hue alone: removed text is underlined and every diff marker
// is bold.
func HighContrastPalette() Palette {
	const (
		white   = "97"
		red     = "91"
		green   = "92"
		yellow  = "93"
		magenta = "95"
		cyan    = "96"
	)
	return Palette{
		Title:    []string{paramBold, white},
		Muted:    []string{white},
		Faint:    []string{white},
		Prompt:   []string{paramBold, cyan},
		User:     []string{paramBold, white},
		Accent:   []string{paramBold, cyan},
		Tool:     []string{paramBold, yellow},
		FilePath: []string{paramBold, paramUnderline, cyan},
		GlyphAdd: []string{paramBold, green},
		MarkAdd:  []string{paramBold, green},
		MarkDel:  []string{paramBold, red},
		Text:     []string{white},
		Added:    []string{paramBold, green},
		Removed:  []string{paramBold, red, paramUnderline},
		Info:     []string{paramBold, cyan},
		Warn:     []string{paramBold, yellow},
		Error:    []string{paramBold, red},
		DiffRule: []string{white},
		SelBG:    []string{paramReverse},

		ThinkingParam: []string{paramItalic, white},

		removedBlock: []string{paramBold, red, paramUnderline},
		addedRow:     rowStyle{num: []string{white}, body: []string{paramBold, green}, text: []string{paramBold, green}},
		removedRow:   rowStyle{num: []string{white}, body: []string{paramBold, red, paramUnderline}, text: []string{paramBold, red, paramUnderline}},
	}
}
