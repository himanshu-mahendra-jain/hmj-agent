package terminal

import (
	"strings"

	"hmj-agent/internal/terminal/theme"
)

// Alignment controls text alignment within table cells.
type Alignment int

const (
	AlignLeft Alignment = iota
	AlignCenter
	AlignRight
)

// Table represents a parsed tabular structure ready for terminal rendering.
type Table struct {
	Headers []string
	Align   []Alignment
	Rows    [][]string
}

func padCell(s string, width int, align Alignment) string {
	w := VisibleWidth(s)
	if w >= width {
		return s
	}
	diff := width - w
	switch align {
	case AlignRight:
		return strings.Repeat(" ", diff) + s
	case AlignCenter:
		left := diff / 2
		right := diff - left
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
	default: // AlignLeft
		return s + strings.Repeat(" ", diff)
	}
}

func isSeparatorRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	// A lone "|" is both prefix and suffix; it needs two pipes to hold cells.
	if len(trimmed) < 2 || !strings.HasPrefix(trimmed, "|") || !strings.HasSuffix(trimmed, "|") {
		return false
	}
	parts := strings.Split(trimmed[1:len(trimmed)-1], "|")
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) == 0 {
			return false
		}
		for _, r := range p {
			if r != '-' && r != ':' && r != ' ' {
				return false
			}
		}
		if !strings.Contains(p, "-") {
			return false
		}
	}
	return true
}

func parseCells(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimSuffix(strings.TrimPrefix(trimmed, "|"), "|")
	rawCells := strings.Split(trimmed, "|")
	cells := make([]string, len(rawCells))
	for i, c := range rawCells {
		// A Tab would jump to the next tab stop and push the cell's border out of line.
		cells[i] = strings.ReplaceAll(strings.TrimSpace(c), "\t", " ")
	}
	return cells
}

func parseAlignment(cell string) Alignment {
	cell = strings.TrimSpace(cell)
	left := strings.HasPrefix(cell, ":")
	right := strings.HasSuffix(cell, ":")
	if left && right {
		return AlignCenter
	}
	if right {
		return AlignRight
	}
	return AlignLeft
}

// ParseMarkdownTable parses a slice of lines into a Table if they form a valid
// GitHub-flavored Markdown table (requiring a header row and a separator row).
func ParseMarkdownTable(lines []string) (*Table, bool) {
	if len(lines) < 2 {
		return nil, false
	}
	if !isSeparatorRow(lines[1]) {
		return nil, false
	}
	headers := parseCells(lines[0])
	sepCells := parseCells(lines[1])
	if len(headers) != len(sepCells) || len(headers) == 0 {
		return nil, false
	}
	align := make([]Alignment, len(sepCells))
	for i, s := range sepCells {
		align[i] = parseAlignment(s)
	}
	var rows [][]string
	for i := 2; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			break
		}
		cells := parseCells(line)
		for len(cells) < len(headers) {
			cells = append(cells, "")
		}
		if len(cells) > len(headers) {
			cells = cells[:len(headers)]
		}
		rows = append(rows, cells)
	}
	return &Table{
		Headers: headers,
		Align:   align,
		Rows:    rows,
	}, true
}

// cellStyle is the markdown style of a run of cell text.
type cellStyle uint8

const (
	stylePlain cellStyle = iota
	styleBold
	styleItalic
	styleCode
	styleLink
)

// cellSpan is a run of visible cell text in one style, and the URL it links to.
type cellSpan struct {
	text  string
	style cellStyle
	url   string
}

// cellLine is one visual line of a cell, as styled spans.
type cellLine []cellSpan

// plain returns the visible text of the line.
func (l cellLine) plain() string {
	var b strings.Builder
	for _, sp := range l {
		b.WriteString(sp.text)
	}
	return b.String()
}

// width returns the display width of the line.
func (l cellLine) width() int {
	w := 0
	for _, sp := range l {
		w += VisibleWidth(sp.text)
	}
	return w
}

// render returns the line as terminal text, styled when useUnicode is set.
func (l cellLine) render(useUnicode bool) string {
	if !useUnicode {
		return l.plain()
	}
	var b strings.Builder
	for _, sp := range l {
		switch sp.style {
		case styleBold:
			b.WriteString(theme.Bold(theme.BrightWhite(sp.text)))
		case styleItalic:
			b.WriteString(theme.Style(sp.text, "3"))
		case styleCode:
			b.WriteString(theme.Hyperlink(sp.url, theme.Accent(sp.text)))
		case styleLink:
			b.WriteString(theme.Hyperlink(sp.url, theme.Style(sp.text, "4")))
		default:
			b.WriteString(sp.text)
		}
	}
	return b.String()
}

// parseCellMarkdown splits s into styled spans. A `code` span keeps its content verbatim,
// asterisks included. **bold** and *italic* need a closing marker, and neither marker may
// touch whitespace on its inner side, so "5 * 3" stays as written. A marker without a match
// is literal text. Markers nested inside bold or italic are dropped and the span takes the
// outer style.
func parseCellMarkdown(s string) cellLine {
	var spans cellLine
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			spans = append(spans, cellSpan{text: plain.String()})
			plain.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch s[i] {
		case '`':
			if end := strings.IndexByte(s[i+1:], '`'); end > 0 {
				flush()
				spans = append(spans, cellSpan{text: s[i+1 : i+1+end], style: styleCode})
				i += end + 2
				continue
			}
		case '[':
			if text, dest, n, ok := markdownLink(s[i:]); ok {
				if webURL, fileURL, path := linkTarget(dest); webURL != "" || fileURL != "" {
					flush()
					// The label's own markers are dropped, as inside bold
					label := parseCellMarkdown(text).plain()
					if webURL != "" {
						spans = append(spans, cellSpan{label, styleLink, webURL})
					} else if spans = append(spans, cellSpan{text: label}); label != path {
						spans = append(spans, cellSpan{text: " ("}, cellSpan{path, styleCode, fileURL}, cellSpan{text: ")"})
					}
					i += n
					continue
				}
			}
		case '*':
			if inner, ok := emphasisInner(s[i:], "**"); ok {
				flush()
				spans = append(spans, cellSpan{text: parseCellMarkdown(inner).plain(), style: styleBold})
				i += len(inner) + 4
				continue
			}
			if inner, ok := emphasisInner(s[i:], "*"); ok {
				flush()
				spans = append(spans, cellSpan{text: parseCellMarkdown(inner).plain(), style: styleItalic})
				i += len(inner) + 2
				continue
			}
		}
		plain.WriteByte(s[i])
		i++
	}
	flush()
	return spans
}

// emphasisInner returns the text between the delim that s starts with and the next closing
// delim, when the opening marker is not followed by whitespace and the closing one is not
// preceded by it.
func emphasisInner(s, delim string) (string, bool) {
	n := len(delim)
	if !strings.HasPrefix(s, delim) || len(s) <= n || isCellSpace(s[n]) || s[n] == '*' {
		return "", false
	}
	for j := n + 1; j+n <= len(s); j++ {
		if strings.HasPrefix(s[j:], delim) && !isCellSpace(s[j-1]) {
			return s[n:j], true
		}
	}
	return "", false
}

// isCellSpace reports whether c is a space or tab.
func isCellSpace(c byte) bool {
	return c == ' ' || c == '\t'
}

// stripCellMarkdown returns the visible text of a cell: what renderCellMarkdown draws, without
// its styling. Column widths are measured on it.
func stripCellMarkdown(s string) string {
	if !strings.ContainsAny(s, "*`[") {
		return s
	}
	return parseCellMarkdown(s).plain()
}

// cellWords splits a line into words at whitespace, keeping each piece's style, so a styled
// span cut by a line break keeps its style on both lines.
func cellWords(l cellLine) []cellLine {
	var words []cellLine
	var cur cellLine
	for _, sp := range l {
		start := -1
		for i := 0; i <= len(sp.text); i++ {
			if i < len(sp.text) && !isCellSpace(sp.text[i]) {
				if start < 0 {
					start = i
				}
				continue
			}
			if start >= 0 {
				cur = append(cur, cellSpan{sp.text[start:i], sp.style, sp.url})
				start = -1
			}
			if i < len(sp.text) && len(cur) > 0 {
				words = append(words, cur)
				cur = nil
			}
		}
	}
	if len(cur) > 0 {
		words = append(words, cur)
	}
	return words
}

// splitCellWord breaks a word into chunks that each fit within width, keeping the style of
// every piece and always taking at least one rune per chunk. It also returns the display width
// of the last chunk.
func splitCellWord(w cellLine, width int) ([]cellLine, int) {
	var chunks []cellLine
	var cur cellLine
	curWidth := 0
	for _, sp := range w {
		var piece strings.Builder
		for _, r := range sp.text {
			rw := VisibleWidth(string(r))
			if curWidth > 0 && curWidth+rw > width {
				if piece.Len() > 0 {
					cur = append(cur, cellSpan{piece.String(), sp.style, sp.url})
					piece.Reset()
				}
				chunks = append(chunks, cur)
				cur, curWidth = nil, 0
			}
			piece.WriteRune(r)
			curWidth += rw
		}
		if piece.Len() > 0 {
			cur = append(cur, cellSpan{piece.String(), sp.style, sp.url})
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks, curWidth
}

// wrapParagraph wraps one newline-free paragraph into lines no wider than width.
func wrapParagraph(p string, width int) []cellLine {
	line := parseCellMarkdown(strings.TrimSpace(p))
	if line.width() <= width {
		return []cellLine{line}
	}

	var result []cellLine
	var cur cellLine
	curWidth := 0
	for _, w := range cellWords(line) {
		wWidth := w.width()
		if len(cur) > 0 && curWidth+1+wWidth <= width {
			cur = append(cur, cellSpan{text: " "})
			cur = append(cur, w...)
			curWidth += 1 + wWidth
			continue
		}
		if len(cur) > 0 {
			result = append(result, cur)
			cur, curWidth = nil, 0
		}
		if wWidth > width {
			// The last chunk starts the next line so following words can join it.
			chunks, lastWidth := splitCellWord(w, width)
			result = append(result, chunks[:len(chunks)-1]...)
			cur, curWidth = chunks[len(chunks)-1], lastWidth
			continue
		}
		cur, curWidth = w, wWidth
	}
	if len(cur) > 0 {
		result = append(result, cur)
	}
	return result
}

// wrapCell breaks text into lines of styled spans so that no line's visible width exceeds
// width.
func wrapCell(text string, width int) []cellLine {
	if width <= 0 {
		return []cellLine{parseCellMarkdown(text)}
	}
	text = strings.ReplaceAll(text, "<br/>", "\n")
	text = strings.ReplaceAll(text, "<br>", "\n")

	var result []cellLine
	for p := range strings.SplitSeq(text, "\n") {
		result = append(result, wrapParagraph(p, width)...)
	}
	return result
}

// naturalColumnWidths returns the widest visible width of each column across the
// header and every row cell.
func naturalColumnWidths(headers []string, rows [][]string) []int {
	numCols := len(headers)
	colWidths := make([]int, numCols)
	for i, h := range headers {
		colWidths[i] = VisibleWidth(stripCellMarkdown(h))
	}
	for _, row := range rows {
		for i, c := range row {
			if i < numCols {
				if w := VisibleWidth(stripCellMarkdown(c)); w > colWidths[i] {
					colWidths[i] = w
				}
			}
		}
	}
	return colWidths
}

// computeColumnWidths returns the column content widths, shrunk to fit maxTableWidth
// when the natural widths are too large.
func computeColumnWidths(headers []string, rows [][]string, maxTableWidth int) []int {
	numCols := len(headers)
	colWidths := naturalColumnWidths(headers, rows)

	overhead := 1 + 3*numCols
	totalNatural := overhead
	for _, w := range colWidths {
		totalNatural += w
	}

	if totalNatural <= maxTableWidth {
		return colWidths
	}

	availContent := maxTableWidth - overhead
	minFloor := numCols * 8
	if availContent < minFloor {
		availContent = minFloor
	}

	fixedWidth, wideIndices, wideNaturalSum := classifyColumnWidths(colWidths)
	if len(wideIndices) == 0 || (availContent-fixedWidth) < len(wideIndices)*10 {
		return scaleColumnWidths(colWidths, availContent)
	}
	return allocateWideColumns(colWidths, wideIndices, wideNaturalSum, availContent-fixedWidth)
}

// classifyColumnWidths sums the narrow columns (width <= 16) into fixedWidth and
// collects the wide column indices along with the sum of their natural widths.
func classifyColumnWidths(colWidths []int) (fixedWidth int, wideIndices []int, wideNaturalSum int) {
	for i, w := range colWidths {
		if w <= 16 {
			fixedWidth += w
		} else {
			wideIndices = append(wideIndices, i)
			wideNaturalSum += w
		}
	}
	return fixedWidth, wideIndices, wideNaturalSum
}

// scaleColumnWidths shrinks every column proportionally to fit availContent, with a
// floor of 8 cells, and hands any leftover cells to the leading columns.
func scaleColumnWidths(colWidths []int, availContent int) []int {
	numCols := len(colWidths)
	res := make([]int, numCols)
	sumAll := 0
	for _, w := range colWidths {
		sumAll += w
	}
	if sumAll == 0 {
		sumAll = 1
	}
	rem := availContent
	for i := range numCols {
		share := max(int(float64(colWidths[i])*float64(availContent)/float64(sumAll)), 8)
		res[i] = share
		rem -= share
	}
	for i := 0; rem > 0 && i < numCols; i++ {
		res[i]++
		rem--
	}
	return res
}

// allocateWideColumns keeps narrow columns at their natural width and splits remWide
// among the wide columns in proportion to their natural widths (floor of 10 cells).
func allocateWideColumns(colWidths, wideIndices []int, wideNaturalSum, remWide int) []int {
	res := make([]int, len(colWidths))
	copy(res, colWidths)
	allocated := 0
	for _, idx := range wideIndices {
		share := max(int(float64(colWidths[idx])*float64(remWide)/float64(wideNaturalSum)), 10)
		res[idx] = share
		allocated += share
	}
	diff := remWide - allocated
	maxIdx := wideIndices[0]
	for _, idx := range wideIndices {
		if res[idx] > res[maxIdx] {
			maxIdx = idx
		}
	}
	res[maxIdx] += diff
	return res
}

// renderTableWidth returns the maximum rendered table width for the current terminal.
func renderTableWidth() int {
	maxTableWidth := getTerminalWidth()
	if maxTableWidth <= 0 {
		maxTableWidth = 100
	} else if maxTableWidth > 140 {
		maxTableWidth = 140
	} else if maxTableWidth > 4 {
		maxTableWidth -= 2
	}
	if maxTableWidth < 40 {
		maxTableWidth = 40
	}
	return maxTableWidth
}

// tableGlyphs holds the characters used to draw a table grid.
type tableGlyphs struct {
	tl, tr, bl, br, tm, bm, mm string
	hl, vl, lm, rm             string
}

// newTableGlyphs returns box-drawing glyphs when useUnicode is set, ASCII otherwise.
func newTableGlyphs(useUnicode bool) tableGlyphs {
	if useUnicode {
		return tableGlyphs{
			tl: "┌", tr: "┐", bl: "└", br: "┘",
			tm: "┬", bm: "┴", mm: "┼",
			hl: "─", vl: "│", lm: "├", rm: "┤",
		}
	}
	return tableGlyphs{
		tl: "+", tr: "+", bl: "+", br: "+",
		tm: "+", bm: "+", mm: "+",
		hl: "-", vl: "|", lm: "+", rm: "+",
	}
}

// writeTableBorder writes one horizontal grid line: the left edge, a run of fill per
// column separated by mid, the right edge, and a newline.
func writeTableBorder(b *strings.Builder, colWidths []int, left, fill, mid, right string) {
	b.WriteString(theme.Faint(left))
	for i, w := range colWidths {
		b.WriteString(theme.Faint(strings.Repeat(fill, w+2)))
		if i < len(colWidths)-1 {
			b.WriteString(theme.Faint(mid))
		}
	}
	b.WriteString(theme.Faint(right) + "\n")
}

// writeTableRowLine writes one visual line of a row: each cell is rendered by render, padded
// to its column width and framed by the vertical glyph vl, followed by a newline.
func writeTableRowLine(b *strings.Builder, lines []cellLine, colWidths []int, align []Alignment, vl string, render func(cellLine) string) {
	b.WriteString(theme.Faint(vl))
	for i := range colWidths {
		b.WriteString(" " + padCell(render(lines[i]), colWidths[i], align[i]) + " " + theme.Faint(vl))
	}
	b.WriteString("\n")
}

// writeTableHeader wraps the header cells and writes every header line.
func writeTableHeader(b *strings.Builder, headers []string, colWidths []int, align []Alignment, g tableGlyphs, useUnicode bool) {
	numCols := len(headers)
	wrappedHeaders := make([][]cellLine, numCols)
	headerHeight := 1
	for i, h := range headers {
		wrappedHeaders[i] = wrapCell(h, colWidths[i])
		if len(wrappedHeaders[i]) > headerHeight {
			headerHeight = len(wrappedHeaders[i])
		}
	}

	// Header cells are bold already, so their own markdown styles are not applied.
	render := func(l cellLine) string {
		if useUnicode {
			return theme.Bold(theme.Accent(l.plain()))
		}
		return l.plain()
	}
	for h := range headerHeight {
		lines := make([]cellLine, numCols)
		for i := range numCols {
			if h < len(wrappedHeaders[i]) {
				lines[i] = wrappedHeaders[i][h]
			}
		}
		writeTableRowLine(b, lines, colWidths, align, g.vl, render)
	}
}

// writeTableData wraps the body cells and writes every row, adding a separator between
// rows when any cell wraps onto multiple lines.
func writeTableData(b *strings.Builder, rows [][]string, colWidths []int, align []Alignment, g tableGlyphs, useUnicode bool) {
	numCols := len(colWidths)
	hasMultiLine := false
	wrappedData := make([][][]cellLine, len(rows))
	for rIdx, row := range rows {
		wrappedData[rIdx] = make([][]cellLine, numCols)
		for i := range numCols {
			c := ""
			if i < len(row) {
				c = row[i]
			}
			wrappedData[rIdx][i] = wrapCell(c, colWidths[i])
			if len(wrappedData[rIdx][i]) > 1 {
				hasMultiLine = true
			}
		}
	}

	render := func(l cellLine) string {
		return l.render(useUnicode)
	}
	for rIdx := range rows {
		if hasMultiLine && rIdx > 0 {
			writeTableBorder(b, colWidths, g.lm, g.hl, g.mm, g.rm)
		}

		rowHeight := 1
		for i := range numCols {
			if len(wrappedData[rIdx][i]) > rowHeight {
				rowHeight = len(wrappedData[rIdx][i])
			}
		}

		for h := range rowHeight {
			lines := make([]cellLine, numCols)
			for i := range numCols {
				if h < len(wrappedData[rIdx][i]) {
					lines[i] = wrappedData[rIdx][i][h]
				}
			}
			writeTableRowLine(b, lines, colWidths, align, g.vl, render)
		}
	}
}

// Render renders the table into an aligned, beautifully formatted box-drawing grid.
func (t *Table) Render() string {
	return t.render(renderTableWidth())
}

// render draws the table in at most maxWidth columns.
func (t *Table) render(maxWidth int) string {
	numCols := len(t.Headers)
	if numCols == 0 {
		return ""
	}

	colWidths := computeColumnWidths(t.Headers, t.Rows, maxWidth)
	useUnicode := theme.Enabled() && !theme.Accessibility()
	g := newTableGlyphs(useUnicode)

	var b strings.Builder
	writeTableBorder(&b, colWidths, g.tl, g.hl, g.tm, g.tr)
	writeTableHeader(&b, t.Headers, colWidths, t.Align, g, useUnicode)
	writeTableBorder(&b, colWidths, g.lm, g.hl, g.mm, g.rm)
	writeTableData(&b, t.Rows, colWidths, t.Align, g, useUnicode)
	writeTableBorder(&b, colWidths, g.bl, g.hl, g.bm, g.br)

	return b.String()
}
