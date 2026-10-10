package diff

import (
	"fmt"
	"strings"
)

// SegmentStyle names a visual role for a painted fragment. The display layer
// maps these onto theme colours; this package stays escape-code free.
type SegmentStyle int

const (
	StylePlain SegmentStyle = iota
	StyleLineNum
	StyleGlyphAdd
	StyleGlyphDel
	StyleTextAdd
	StyleTextDel
	StyleMarkAdd
	StyleMarkDel
	StyleRule
	StyleFaint
	StyleAccent
	StylePath
)

// PaintFunc styles a fragment according to its role.
type PaintFunc func(s string, kind SegmentStyle) string

// PlainPaint is the identity painter used when colour is disabled.
func PlainPaint(s string, _ SegmentStyle) string { return s }

// Row is one fully-assembled display row. Alignment happens on plain text
// first (Number/Glyph/Body are already padded), so column maths never has to
// count escape bytes.
type Row struct {
	Number string // painted old/new gutter
	Glyph  string // painted "+", "-" or blank
	Body   string // painted line content
	Raw    string // unstyled equivalent, for tests and plain output
	Kind   Kind
	// Notice marks rows the renderer adds itself (separators, truncation notes), as
	// opposed to file content.
	Notice bool
}

// Render turns the diff into display rows. paint may be nil (plain text).
func (f *File) Render(paint PaintFunc) []Row {
	if f == nil {
		return nil
	}
	if paint == nil {
		paint = PlainPaint
	}

	gw := f.MaxNumber()

	var rows []Row
	// File header: path + change statistics.
	stats := f.StatSummary()
	headerPad := strings.Repeat(" ", gw+1)
	rows = append(rows, Row{
		Number: paint(headerPad, StyleLineNum),
		Glyph:  paint(" ", StyleFaint),
		Body:   paint(f.Path, StylePath) + paint("  "+stats, StyleAccent),
		Raw:    f.Path + "  " + stats,
	})
	// Hunk headers live under the same gutter.
	hunkPad := strings.Repeat(" ", gw+1)
	for i, h := range f.Hunks {
		if i > 0 {
			rows = append(rows, Row{Glyph: paint("···", StyleFaint), Raw: "···", Notice: true})
		}
		rows = append(rows, Row{
			Number: paint(hunkPad, StyleLineNum),
			Glyph:  paint("@", StyleAccent),
			Body:   paint(h.HeaderOf(), StyleAccent),
			Raw:    h.HeaderOf(),
		})
		for _, l := range h.Lines {
			rows = append(rows, renderLine(l, gw, paint))
		}
	}

	if f.NewlineMissingOld || f.NewlineMissingNew {
		rows = append(rows, Row{
			Glyph: paint(" ", StyleFaint),
			Body:  paint("\\ No newline at end of file", StyleFaint),
			Raw:   "\\ No newline at end of file",
		})
	}

	if f.Truncated && f.Omitted > 0 {
		note := fmt.Sprintf("… %d more changed lines not shown", f.Omitted)
		rows = append(rows, Row{Glyph: paint("…", StyleFaint), Body: paint(note, StyleFaint), Raw: note, Notice: true})
	}
	return rows
}

// Plain renders the diff as an escape-free string (tests, logs, pipe output).
func (f *File) Plain() string {
	var b strings.Builder
	for _, r := range f.Render(nil) {
		line := r.Number + r.Glyph + r.Body
		if line == "" {
			line = r.Raw
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderLine assembles one row with unified, vertically aligned line numbers.
func renderLine(l Line, gw int, paint PaintFunc) Row {
	switch l.Kind {
	case Del:
		num := fmt.Sprintf("%*d ", gw, l.Old)
		return Row{
			Number: paint(num, StyleLineNum),
			Glyph:  paint("-", StyleGlyphDel),
			Body:   paintLineBody(l, paint, StyleTextDel, StyleMarkDel),
			Raw:    fmt.Sprintf("%s- %s", num, l.Text),
			Kind:   Del,
		}
	case Add:
		num := fmt.Sprintf("%*d ", gw, l.New)
		return Row{
			Number: paint(num, StyleLineNum),
			Glyph:  paint("+", StyleGlyphAdd),
			Body:   paintLineBody(l, paint, StyleTextAdd, StyleMarkAdd),
			Raw:    fmt.Sprintf("%s+ %s", num, l.Text),
			Kind:   Add,
		}
	default:
		lineNum := l.New
		if lineNum == 0 {
			lineNum = l.Old
		}
		num := fmt.Sprintf("%*d ", gw, lineNum)
		return Row{
			Number: paint(num, StyleLineNum),
			Glyph:  paint(" ", StyleFaint),
			Body:   paint(l.Text, StylePlain),
			Raw:    num + " " + l.Text,
			Kind:   Ctx,
		}
	}
}

// paintLineBody renders the body, applying inline emphasis when Segs exist.
func paintLineBody(l Line, paint PaintFunc, textStyle, markStyle SegmentStyle) string {
	if len(l.Segs) == 0 {
		return paint(l.Text, textStyle)
	}
	var b strings.Builder
	for _, s := range l.Segs {
		if s.Changed {
			b.WriteString(paint(s.Text, markStyle))
			continue
		}
		b.WriteString(paint(s.Text, textStyle))
	}
	return b.String()
}

// HeaderOf renders an "@@ -old,count +new,count @@" header string.
func (h Hunk) HeaderOf() string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
}

// StatSummary returns a human summary such as "+5 / -5 lines".
func (f *File) StatSummary() string {
	switch {
	case f.Added == 0 && f.Removed == 0:
		return "no changes"
	case f.Removed == 0:
		return fmt.Sprintf("+%d lines", f.Added)
	case f.Added == 0:
		return fmt.Sprintf("-%d lines", f.Removed)
	default:
		return fmt.Sprintf("+%d / -%d lines", f.Added, f.Removed)
	}
}
