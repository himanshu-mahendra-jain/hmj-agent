package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"hmj-agent/internal/terminal"
)

// vtScreen is a small VT100/xterm emulator for tests. It applies what hmj writes to a grid of
// cells, so a test can check what a user would actually see: which text is on which row after
// redraws and erases, where the cursor is, and whether the screen or scrollback was cleared.
// It covers the sequences hmj writes: cursor movement (CUU/CUD/CUF/CUB/CNL/CPL/CHA/CUP),
// erase in display and line (ED/EL), SGR (ignored), DECSET 25/2004, save/restore cursor and
// OSC strings (recorded, not drawn). Other sequences are parsed and ignored.
type vtScreen struct {
	rows, cols    int
	grid          [][]rune // 0 is a blank cell; wideTail marks the right half of a wide rune
	row, col      int
	pendingWrap   bool // the last column was written: the next rune wraps first (xterm)
	savedRow      int
	savedCol      int
	cursorHidden  bool
	bracketed     bool     // bracketed paste mode is on
	scrollback    []string // rows scrolled off the top, oldest first
	oscs          []string // OSC payloads in order, e.g. "133;A"
	clearedScroll bool     // ED 3 (erase scrollback) was received
}

const wideTail rune = -1

// screenResize is a terminal size change at a byte offset of the output, as Resize records it.
type screenResize struct {
	offset     int
	rows, cols int
}

// renderScreen replays output on a fresh rows x cols screen, applying each resize when the
// output reaches its offset.
func renderScreen(output string, rows, cols int, resizes []screenResize) *vtScreen {
	v := &vtScreen{rows: rows, cols: cols}
	v.grid = make([][]rune, rows)
	for i := range v.grid {
		v.grid[i] = make([]rune, cols)
	}
	pos := 0
	for _, rz := range resizes {
		v.feed(output[pos:min(rz.offset, len(output))])
		pos = min(rz.offset, len(output))
		v.resize(rz.rows, rz.cols)
	}
	v.feed(output[pos:])
	return v
}

// feed parses s. A sequence cut off at the end of s is dropped; callers replay whole output.
func (v *vtScreen) feed(s string) {
	for i := 0; i < len(s); {
		switch b := s[i]; {
		case b == 0x1b:
			i = v.escape(s, i)
		case b == '\r':
			v.col, v.pendingWrap = 0, false
			i++
		case b == '\n':
			v.lineFeed()
			i++
		case b == '\b':
			if v.col > 0 {
				v.col--
			}
			v.pendingWrap = false
			i++
		case b == '\t':
			v.col = min(v.cols-1, (v.col/8+1)*8)
			i++
		case b < 0x20 || b == 0x7f:
			i++ // BEL and other controls draw nothing
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			v.put(r)
			i += size
		}
	}
}

// escape handles the sequence starting with ESC at s[i] and returns the index after it.
func (v *vtScreen) escape(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[':
		j := i + 2
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		if j >= len(s) {
			return len(s)
		}
		v.csi(s[i+2:j], s[j])
		return j + 1
	case ']':
		j := i + 2
		for j < len(s) {
			if s[j] == 0x07 {
				v.oscs = append(v.oscs, s[i+2:j])
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				v.oscs = append(v.oscs, s[i+2:j])
				return j + 2
			}
			j++
		}
		return len(s)
	case '7':
		v.savedRow, v.savedCol = v.row, v.col
		return i + 2
	case '8':
		v.row, v.col, v.pendingWrap = v.savedRow, v.savedCol, false
		return i + 2
	case '(', ')':
		return min(i+3, len(s)) // character set designation: ESC ( B
	default:
		return i + 2
	}
}

// csi applies one control sequence with its parameter bytes and final byte.
func (v *vtScreen) csi(params string, final byte) {
	private := strings.HasPrefix(params, "?")
	params = strings.TrimLeft(params, "?<>=")
	var nums []int
	for p := range strings.SplitSeq(params, ";") {
		n, _ := strconv.Atoi(p)
		nums = append(nums, n)
	}
	arg := func(i, def int) int {
		if i < len(nums) && nums[i] > 0 {
			return nums[i]
		}
		return def
	}
	if final != 'm' {
		v.pendingWrap = false
	}
	switch final {
	case 'A':
		v.row = max(0, v.row-arg(0, 1))
	case 'B':
		v.row = min(v.rows-1, v.row+arg(0, 1))
	case 'C':
		v.col = min(v.cols-1, v.col+arg(0, 1))
	case 'D':
		v.col = max(0, v.col-arg(0, 1))
	case 'E':
		v.row, v.col = min(v.rows-1, v.row+arg(0, 1)), 0
	case 'F':
		v.row, v.col = max(0, v.row-arg(0, 1)), 0
	case 'G':
		v.col = min(v.cols-1, arg(0, 1)-1)
	case 'H', 'f':
		v.row, v.col = min(v.rows-1, arg(0, 1)-1), min(v.cols-1, arg(1, 1)-1)
	case 'J':
		v.eraseDisplay(arg(0, 0))
	case 'K':
		v.eraseLine(arg(0, 0))
	case 's':
		v.savedRow, v.savedCol = v.row, v.col
	case 'u':
		v.row, v.col = v.savedRow, v.savedCol
	case 'h', 'l':
		if private {
			on := final == 'h'
			for _, n := range nums {
				switch n {
				case 25:
					v.cursorHidden = !on
				case 2004:
					v.bracketed = on
				}
			}
		}
	}
}

// put draws r at the cursor, wrapping at the right margin the way xterm does.
func (v *vtScreen) put(r rune) {
	w := terminal.VisibleWidth(string(r))
	if w == 0 {
		return // combining marks and zero-width joiners add nothing to the grid
	}
	if v.pendingWrap || v.col+w > v.cols {
		v.col, v.pendingWrap = 0, false
		v.lineFeed()
	}
	v.grid[v.row][v.col] = r
	if w == 2 && v.col+1 < v.cols {
		v.grid[v.row][v.col+1] = wideTail
	}
	v.col += w
	if v.col >= v.cols {
		v.col, v.pendingWrap = v.cols-1, true
	}
}

// lineFeed moves down a row, scrolling the screen up into the scrollback at the bottom.
func (v *vtScreen) lineFeed() {
	if v.row < v.rows-1 {
		v.row++
		return
	}
	v.scrollback = append(v.scrollback, v.lineText(0))
	copy(v.grid, v.grid[1:])
	v.grid[v.rows-1] = make([]rune, v.cols)
}

func (v *vtScreen) eraseDisplay(mode int) {
	switch mode {
	case 0:
		v.eraseLine(0)
		for r := v.row + 1; r < v.rows; r++ {
			v.grid[r] = make([]rune, v.cols)
		}
	case 1:
		v.eraseLine(1)
		for r := range v.row {
			v.grid[r] = make([]rune, v.cols)
		}
	case 2:
		for r := range v.grid {
			v.grid[r] = make([]rune, v.cols)
		}
	case 3:
		v.scrollback, v.clearedScroll = nil, true
	}
}

func (v *vtScreen) eraseLine(mode int) {
	from, to := v.col, v.cols // 0: cursor to end of line
	switch mode {
	case 1:
		from, to = 0, v.col+1
	case 2:
		from, to = 0, v.cols
	}
	for c := from; c < min(to, v.cols); c++ {
		v.grid[v.row][c] = 0
	}
}

// resize changes the size, keeping the top-left content (no reflow) and the cursor in range.
func (v *vtScreen) resize(rows, cols int) {
	grid := make([][]rune, rows)
	for r := range grid {
		grid[r] = make([]rune, cols)
		if r < v.rows {
			copy(grid[r], v.grid[r])
		}
	}
	v.grid, v.rows, v.cols = grid, rows, cols
	v.row, v.col, v.pendingWrap = min(v.row, rows-1), min(v.col, cols-1), false
}

// lineText returns row r without trailing blanks.
func (v *vtScreen) lineText(r int) string {
	var b strings.Builder
	for _, c := range v.grid[r] {
		switch c {
		case wideTail:
		case 0:
			b.WriteByte(' ')
		default:
			b.WriteRune(c)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// Lines returns every row of the screen without trailing blanks.
func (v *vtScreen) Lines() []string {
	out := make([]string, v.rows)
	for r := range out {
		out[r] = v.lineText(r)
	}
	return out
}

// Text returns the visible screen as one string, rows joined with newlines, trailing empty
// rows removed.
func (v *vtScreen) Text() string {
	return strings.TrimRight(strings.Join(v.Lines(), "\n"), "\n")
}

// Cursor returns the cursor row and column, both zero-based.
func (v *vtScreen) Cursor() (row, col int) { return v.row, v.col }

// RowOf returns the last row containing substr, or -1.
func (v *vtScreen) RowOf(substr string) int {
	for r := v.rows - 1; r >= 0; r-- {
		if strings.Contains(v.lineText(r), substr) {
			return r
		}
	}
	return -1
}

// Dump draws the screen in a frame with the cursor shown as █, for failure messages.
func (v *vtScreen) Dump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "screen %dx%d, cursor row %d col %d:\n", v.rows, v.cols, v.row, v.col)
	b.WriteString("+" + strings.Repeat("-", v.cols) + "+\n")
	for r := range v.rows {
		b.WriteByte('|')
		for c := range v.cols {
			switch ch := v.grid[r][c]; {
			case r == v.row && c == v.col:
				b.WriteRune('█')
			case ch == wideTail:
			case ch == 0:
				b.WriteByte(' ')
			default:
				b.WriteRune(ch)
			}
		}
		b.WriteString("|\n")
	}
	b.WriteString("+" + strings.Repeat("-", v.cols) + "+")
	return b.String()
}
