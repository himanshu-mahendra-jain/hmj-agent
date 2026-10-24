package terminal

import (
	"hmj-agent/internal/terminal/theme"
	"unicode"
)

func isWideMiscSymbol(r rune) bool {
	switch {
	case r == 0x2614 || r == 0x2615:
		return true
	case r >= 0x2648 && r <= 0x2653:
		return true
	case r == 0x267f || r == 0x2693 || r == 0x26a1:
		return true
	case r == 0x26aa || r == 0x26ab || r == 0x26bd || r == 0x26be:
		return true
	case r == 0x26c4 || r == 0x26c5 || r == 0x26ce || r == 0x26d4:
		return true
	case r == 0x26ea || (r >= 0x26f2 && r <= 0x26f3) || r == 0x26f5 || r == 0x26fa || r == 0x26fd:
		return true
	case r == 0x2705 || r == 0x270a || r == 0x270b || r == 0x2728:
		return true
	case r == 0x274c || r == 0x274e || (r >= 0x2753 && r <= 0x2755) || r == 0x2757:
		return true
	case (r >= 0x2795 && r <= 0x2797) || r == 0x27b0 || r == 0x27bf:
		return true
	}
	return false
}

// isZeroWidth reports whether r takes no column of its own: combining marks (accents,
// variation selectors), Hangul medial vowels and final consonants, and invisible format
// characters such as the zero-width joiner. The soft hyphen is a format character that
// terminals draw, so it keeps its column.
func isZeroWidth(r rune) bool {
	if r >= 0x1160 && r <= 0x11ff {
		return true
	}
	return r != 0xad && unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf)
}

// isWideEmoji reports whether r is a pictograph terminals draw two columns wide (East
// Asian Width W) outside the blocks runeWidth checks by range.
func isWideEmoji(r rune) bool {
	switch {
	case r == 0x231a || r == 0x231b || (r >= 0x23e9 && r <= 0x23ec) || r == 0x23f0 || r == 0x23f3:
		return true
	case r == 0x25fd || r == 0x25fe || r == 0x2b1b || r == 0x2b1c || r == 0x2b50 || r == 0x2b55:
		return true
	case r == 0x1f004 || r == 0x1f0cf || r == 0x1f18e || (r >= 0x1f191 && r <= 0x1f19a):
		return true
	case r >= 0x1f200 && r <= 0x1f265: // enclosed ideographic supplement
		return true
	case r >= 0x1f680 && r <= 0x1f6ff: // transport and map symbols
		return true
	case (r >= 0x1f7e0 && r <= 0x1f7eb) || r == 0x1f7f0: // coloured circles and squares
		return true
	case r >= 0x1fa70 && r <= 0x1faff: // symbols and pictographs extended-A
		return true
	}
	return false
}

// runeWidth calculates display columns for a single rune.
func runeWidth(r rune) int {
	if r < 0x20 {
		return 0
	}
	if r < 0x7f {
		return 1
	}
	if isZeroWidth(r) {
		return 0
	}
	if isWideEmoji(r) {
		return 2
	}
	if r >= 0x2600 && r <= 0x27bf {
		if isWideMiscSymbol(r) {
			return 2
		}
		return 1
	}
	if r >= 0x1100 && (r <= 0x115f ||
		r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f64f) ||
		(r >= 0x1f900 && r <= 0x1f9ff) ||
		(r >= 0x20000 && r <= 0x2fffd) || // CJK extensions B and later
		(r >= 0x30000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

// VisibleWidth calculates the displayed column width of a string in a terminal,
// stripping ANSI escape sequences and accounting for double-width runes (emojis, CJK).
func VisibleWidth(s string) int {
	clean := theme.StripANSI(s)
	width := 0
	for _, r := range clean {
		width += runeWidth(r)
	}
	return width
}
