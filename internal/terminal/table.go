package terminal

import (
	"strings"
	"unicode"

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
	if strings.HasPrefix(trimmed, "|") {
		trimmed = trimmed[1:]
	}
	if strings.HasSuffix(trimmed, "|") {
		trimmed = trimmed[:len(trimmed)-1]
	}
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
)

// cellSpan is a run of visible cell text in one style.
type cellSpan struct {
	text  string
	style cellStyle
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
			b.WriteString(theme.Accent(sp.text))
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
			spans = append(spans, cellSpan{plain.String(), stylePlain})
			plain.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch s[i] {
		case '`':
			if end := strings.IndexByte(s[i+1:], '`'); end > 0 {
				flush()
				spans = append(spans, cellSpan{s[i+1 : i+1+end], styleCode})
				i += end + 2
				continue
			}
		case '*':
			if inner, ok := emphasisInner(s[i:], "**"); ok {
				flush()
				spans = append(spans, cellSpan{parseCellMarkdown(inner).plain(), styleBold})
				i += len(inner) + 4
				continue
			}
			if inner, ok := emphasisInner(s[i:], "*"); ok {
				flush()
				spans = append(spans, cellSpan{parseCellMarkdown(inner).plain(), styleItalic})
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
	if !strings.ContainsAny(s, "*`") {
		return s
	}
	return parseCellMarkdown(s).plain()
}

// renderCellMarkdown turns **bold**, *italic* and `code` into styled terminal text. Its visible
// text is always exactly stripCellMarkdown(s), so a cell never outgrows its column.
func renderCellMarkdown(s string, useUnicode bool) string {
	return parseCellMarkdown(s).render(useUnicode)
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
				cur = append(cur, cellSpan{sp.text[start:i], sp.style})
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
					cur = append(cur, cellSpan{piece.String(), sp.style})
					piece.Reset()
				}
				chunks = append(chunks, cur)
				cur, curWidth = nil, 0
			}
			piece.WriteRune(r)
			curWidth += rw
		}
		if piece.Len() > 0 {
			cur = append(cur, cellSpan{piece.String(), sp.style})
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
			cur = append(cur, cellSpan{" ", stylePlain})
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
	numCols := len(t.Headers)
	if numCols == 0 {
		return ""
	}

	colWidths := computeColumnWidths(t.Headers, t.Rows, renderTableWidth())
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

// TableStreamer intercepts streaming markdown text chunks to detect, buffer,
// and beautifully render tables upon completion while letting normal prose pass instantly.
type TableStreamer struct {
	out        func(string)
	lineBuf    strings.Builder
	tableLines []string
	inTable    bool
}

// NewTableStreamer creates a TableStreamer sending rendered text to out.
func NewTableStreamer(out func(string)) *TableStreamer {
	return &TableStreamer{out: out}
}

func isPotentialTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	return len(trimmed) >= 2 && strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|")
}

func (s *TableStreamer) processLine(line string) {
	if isPotentialTableRow(line) {
		s.tableLines = append(s.tableLines, line)
		s.inTable = true
		return
	}

	if s.inTable {
		s.flushTable()
	}
	s.out(line + "\n")
}

func (s *TableStreamer) flushTable() {
	if len(s.tableLines) == 0 {
		s.inTable = false
		return
	}
	if tbl, ok := ParseMarkdownTable(s.tableLines); ok {
		s.out(tbl.Render())
	} else {
		for _, l := range s.tableLines {
			s.out(l + "\n")
		}
	}
	s.tableLines = nil
	s.inTable = false
}

// Write processes a streamed chunk of text from the assistant.
func (s *TableStreamer) Write(chunk string) {
	s.lineBuf.WriteString(chunk)
	content := s.lineBuf.String()
	for {
		idx := strings.IndexByte(content, '\n')
		if idx == -1 {
			break
		}
		line := content[:idx]
		content = content[idx+1:]
		s.processLine(line)
	}
	s.lineBuf.Reset()
	s.lineBuf.WriteString(content)
}

// Flush flushes any pending buffered lines at the end of generation.
func (s *TableStreamer) Flush() {
	if s.lineBuf.Len() > 0 {
		line := s.lineBuf.String()
		s.lineBuf.Reset()
		if isPotentialTableRow(line) {
			// Open the table as processLine would, or the row would never be written.
			s.tableLines = append(s.tableLines, line)
			s.inTable = true
		} else {
			if s.inTable {
				s.flushTable()
			}
			s.out(line)
		}
	}
	if s.inTable {
		s.flushTable()
	}
}
