package theme

import (
	"fmt"
	"strconv"
	"strings"
)

// AddGlyph / DelGlyph paint the +/- markers that lead diff rows. The markers are bold
// in every palette so a diff stays readable without colour.
func AddGlyph() string { return Style("+", pal.MarkAdd...) }
func DelGlyph() string { return Style("-", pal.MarkDel...) }

// Num renders a gutter line number. Widths are applied to the plain digits
// before styling so column alignment is identical with and without colour.
func Num(n int, width int) string {
	d := strconv.Itoa(n)
	if len(d) < width {
		d = strings.Repeat(" ", width-len(d)) + d
	}
	return Style(d, pal.LineNum...)
}

// AddedBlock paints whole added text on a subtle dark block.
func AddedBlock(s string) string { return Style(s, pal.addedBlock...) }

// RemovedBlock paints whole removed text on a solid red block.
func RemovedBlock(s string) string { return Style(s, pal.removedBlock...) }

// RemovedLine renders a full-row deletion with dark red background (#5f0000).
func RemovedLine(num, glyph, body string) string {
	return renderRow(num, glyph, body, "-", pal.MarkDel, pal.removedRow)
}

// AddedLine renders a full-row addition with subtle dark charcoal background (#1c1c1c).
func AddedLine(num, glyph, body string) string {
	return renderRow(num, glyph, body, "+", pal.MarkAdd, pal.addedRow)
}

// renderRow lays out one diff row. The glyph falls back to fallback when empty, so every
// row carries its +/- marker with or without colour.
func renderRow(num, glyph, body, fallback string, mark []string, row rowStyle) string {
	cleanNum := strings.TrimRight(StripANSI(num), " ")
	cleanGlyph := strings.TrimSpace(StripANSI(glyph))
	if cleanGlyph == "" {
		cleanGlyph = fallback
	}
	cleanBody := strings.TrimRight(body, " \t\r\n")
	if !enabled {
		return fmt.Sprintf("%s %s %s", cleanNum, cleanGlyph, StripANSI(cleanBody))
	}
	// Restore row background and text after any embedded reset from inline marks.
	restore := sgr(append(append([]string{}, row.bg...), row.text...)...)
	restoredBody := strings.ReplaceAll(cleanBody, "\x1b[0m", "\x1b[0m"+restore)
	restoredBody = strings.ReplaceAll(restoredBody, "\x1b[m", "\x1b[m"+restore)
	open := ""
	if len(row.body) > 0 {
		open = sgr(append(append([]string{}, row.bg...), row.body...)...)
	}
	return sgr(append(append([]string{}, row.bg...), row.num...)...) + cleanNum + " " +
		sgr(mark...) + cleanGlyph + " " + open + restoredBody + "  " + "\x1b[" + resetParam + "m"
}

// CountAdd / CountDel render "+n" / "−n" change statistics.
func CountAdd(n int) string { return Style("+"+strconv.Itoa(n), pal.GlyphAdd...) }
func CountDel(n int) string { return Style(ASCII("−")+strconv.Itoa(n), pal.GlyphDel...) }

// DiffHeader renders a diff title: path, faded separator, then stats.
func DiffHeader(path, stats string) string {
	sep := Faint("·")
	if Accessibility() {
		sep = "|"
	}
	return fmt.Sprintf("%s %s %s", FilePath(path), sep, stats)
}

// FormatDiffStats formats change stats as "+A / -R lines" with +A in green and -R in red.
func FormatDiffStats(added, removed int) string {
	if Accessibility() {
		switch {
		case added == 0 && removed == 0:
			return "[no changes]"
		case removed == 0:
			return fmt.Sprintf("[+%d lines]", added)
		case added == 0:
			return fmt.Sprintf("[-%d lines]", removed)
		default:
			return fmt.Sprintf("[+%d / -%d lines]", added, removed)
		}
	}
	if !enabled {
		switch {
		case added == 0 && removed == 0:
			return "no changes"
		case removed == 0:
			return fmt.Sprintf("+%d lines", added)
		case added == 0:
			return fmt.Sprintf("-%d lines", removed)
		default:
			return fmt.Sprintf("+%d / -%d lines", added, removed)
		}
	}
	switch {
	case added == 0 && removed == 0:
		return Muted("no changes")
	case removed == 0:
		return fmt.Sprintf("%s lines", Added(fmt.Sprintf("+%d", added)))
	case added == 0:
		return fmt.Sprintf("%s lines", Removed(fmt.Sprintf("-%d", removed)))
	default:
		return fmt.Sprintf("%s / %s lines", Added(fmt.Sprintf("+%d", added)), Removed(fmt.Sprintf("-%d", removed)))
	}
}
