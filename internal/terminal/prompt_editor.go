package terminal

import (
	"fmt"
	"hmj-agent/internal/terminal/theme"
	"os"
	"slices"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/term"
)

// Semantic prompt marks (OSC 133). Terminals with shell integration, such as kitty, Ghostty,
// WezTerm and iTerm2, use them to find the prompt: clicking inside it can move the cursor
// (the terminal sends arrow keys), and prompts and replies can be jumped between or selected.
// Other terminals ignore them.
const (
	markPromptStart    = "\x1b]133;A\x07"
	markSecondaryStart = "\x1b]133;A;k=s\x07" // a continuation line of a multi-line prompt
	markInputStart     = "\x1b]133;B\x07"
	markOutputStart    = "\x1b]133;C\x07"
)

// promptMarksEnabled reports whether to write OSC 133 marks: only to a real terminal, not in
// accessibility mode (its line-mode input would receive click-to-move arrows as text), and
// not on the Linux console or a dumb terminal.
func promptMarksEnabled() bool {
	switch os.Getenv("TERM") {
	case "linux", "dumb":
		return false
	}
	return !theme.Accessibility() && isTerminal(os.Stdout.Fd())
}

// markedPrompt wraps a prompt prefix in OSC 133 marks when they are enabled: the prompt
// start before it and the input start after it. secondary marks a continuation line.
func markedPrompt(prefix string, secondary bool) string {
	if !promptMarksEnabled() {
		return prefix
	}
	start := markPromptStart
	if secondary {
		start = markSecondaryStart
	}
	return start + prefix + markInputStart
}

// PromptPrefix returns the styled prompt glyph that starts every prompt, with its OSC 133
// marks when they are enabled.
func PromptPrefix() string {
	return markedPrompt(theme.Prompt("> "), false)
}

func getTerminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	if w, _, err := term.GetSize(int(os.Stdin.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

// rowWidth is the number of columns a redrawn row may use. The last column is left free so a
// full row never leaves the cursor in the terminal's pending-wrap state.
func rowWidth() int {
	return max(getTerminalWidth()-1, 1)
}

// clipEnd shortens plain text s to at most width columns, ending it with "…" when cut.
// Rows that are redrawn in place must not wrap, or moving the cursor up by the row count
// lands short and leaves a stale copy of the top rows on screen.
func clipEnd(s string, width int) string {
	if VisibleWidth(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	var sb strings.Builder
	used := 0
	for _, r := range s {
		w := runeWidth(r)
		if used+w > width-1 {
			break
		}
		sb.WriteRune(r)
		used += w
	}
	sb.WriteString("…")
	return sb.String()
}

// clipStart is clipEnd keeping the end of s, for text being typed where the cursor sits.
func clipStart(s string, width int) string {
	if VisibleWidth(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	rs := []rune(s)
	used, i := 0, len(rs)
	for i > 0 && used+runeWidth(rs[i-1]) <= width-1 {
		i--
		used += runeWidth(rs[i])
	}
	return "…" + string(rs[i:])
}

var (
	globalPromptHistory []string
	historyMu           sync.Mutex
)

// AddPromptHistory records a user prompt into the global prompt history.
func AddPromptHistory(prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	if len(globalPromptHistory) > 0 && globalPromptHistory[len(globalPromptHistory)-1] == prompt {
		return
	}
	globalPromptHistory = append(globalPromptHistory, prompt)
}

// SetPromptHistory replaces the global prompt history.
func SetPromptHistory(prompts []string) {
	historyMu.Lock()
	defer historyMu.Unlock()
	globalPromptHistory = nil
	for _, p := range prompts {
		p = strings.TrimSpace(p)
		if p != "" {
			globalPromptHistory = append(globalPromptHistory, p)
		}
	}
}

// GetPromptHistory returns a copy of the prompt history.
func GetPromptHistory() []string {
	historyMu.Lock()
	defer historyMu.Unlock()
	cp := make([]string, len(globalPromptHistory))
	copy(cp, globalPromptHistory)
	return cp
}

type PromptEditor struct {
	runes             []rune
	cursor            int
	prompt            string
	indent            string
	renderedVisualRow int // cursor's visual row offset from top of prompt (0-based)
	renderedTotalRows int // total visual rows rendered
	renderedLine      int // cursor's current line index (0-based)
	renderedLines     int // total lines rendered minus 1 (index of last rendered line)
	width             int // optional fixed width for testing or overrides; 0 = auto-detect
	historyIndex      int // 0 = current draft, 1 = 1 back, etc.
	savedDraft        string
	stale             bool // the text changed since the last Render
}

func NewPromptEditor(initial string) *PromptEditor {
	ed := &PromptEditor{
		prompt: "> ",
		indent: "  ",
	}
	if len(initial) > 0 {
		ed.runes = []rune(initial)
		ed.cursor = len(ed.runes)
		ed.Render()
	}
	return ed
}

func (e *PromptEditor) TerminalWidth() int {
	if e.width > 0 {
		return e.width
	}
	return getTerminalWidth()
}

func (e *PromptEditor) Layout() (lines []string, curLine, curCol int) {
	s := string(e.runes)
	lines = strings.Split(s, "\n")

	pos := 0
	found := false
	for i, l := range lines {
		runeLen := len([]rune(l))
		if !found && e.cursor <= pos+runeLen {
			curLine = i
			curCol = e.cursor - pos
			found = true
		}
		pos += runeLen + 1
	}
	if !found {
		curLine = len(lines) - 1
		curCol = len([]rune(lines[len(lines)-1]))
	}
	return lines, curLine, curCol
}

type VisualLayoutInfo struct {
	Lines           []string
	CurLine         int
	CurCol          int
	CursorVisualRow int
	CursorVisualCol int
	TotalVisualRows int
}

// wrapPromptLine lays out one prompt line, prefix first, the way a terminal wraps it at width
// columns: a double-width character (CJK, most emoji) takes two columns and moves to the next
// row whole when only one column is left. It returns the number of rows the line takes and
// the row and column of the cursor when it sits before rune cursor (or at the end of the
// line). A cursor right after a full last row reports column width, as the terminal keeps it
// on that row until the next character.
func wrapPromptLine(prefix string, line []rune, cursor, width int) (rows, cursorRow, cursorCol int) {
	col := VisibleWidth(prefix)
	for i, r := range line {
		w := runeWidth(r)
		if r < 0x20 {
			w = 1 // a Tab or other control is drawn as one space (see promptDisplayLine)
		}
		if col+w > width {
			rows++
			col = 0
		}
		if i == cursor {
			cursorRow, cursorCol = rows, col
		}
		col += w
	}
	if cursor >= len(line) {
		cursorRow, cursorCol = rows, col
	}
	return rows + 1, cursorRow, cursorCol
}

func (e *PromptEditor) VisualLayout(width int) VisualLayoutInfo {
	if width <= 0 {
		width = 80
	}

	lines, curLine, curCol := e.Layout()
	var (
		totalVisualRows int
		cursorVisualRow int
		cursorVisualCol int
	)

	for i, l := range lines {
		prefix := e.indent
		if i == 0 {
			prefix = e.prompt
		}
		lineRows, rowAtCursor, colAtCursor := wrapPromptLine(prefix, []rune(l), curCol, width)

		if i < curLine {
			cursorVisualRow += lineRows
		} else if i == curLine {
			cursorVisualRow += rowAtCursor
			cursorVisualCol = colAtCursor
		}

		totalVisualRows += lineRows
	}

	if totalVisualRows < 1 {
		totalVisualRows = 1
	}

	return VisualLayoutInfo{
		Lines:           lines,
		CurLine:         curLine,
		CurCol:          curCol,
		CursorVisualRow: cursorVisualRow,
		CursorVisualCol: cursorVisualCol,
		TotalVisualRows: totalVisualRows,
	}
}

// Render redraws the prompt. The frame is written in one piece so the terminal never
// shows it half drawn.
func (e *PromptEditor) Render() {
	width := e.TerminalWidth()
	vl := e.VisualLayout(width)

	if isTerminal(os.Stdout.Fd()) {
		var frame strings.Builder
		if e.renderedVisualRow > 0 {
			fmt.Fprintf(&frame, "\x1b[%dA", e.renderedVisualRow)
		}
		frame.WriteString("\r\x1b[J")

		for i, l := range vl.Lines {
			prefix := markedPrompt(e.indent, true)
			if i == 0 {
				prefix = markedPrompt(e.prompt, false)
			}
			frame.WriteString(prefix + promptDisplayLine(l))
			if i < len(vl.Lines)-1 {
				frame.WriteString("\r\n") // \r\n ensures col 0 on next line in raw mode
			}
		}

		lastVisualRow := vl.TotalVisualRows - 1
		if up := lastVisualRow - vl.CursorVisualRow; up > 0 {
			fmt.Fprintf(&frame, "\x1b[%dA", up)
		}

		frame.WriteString("\r")
		if vl.CursorVisualCol > 0 {
			fmt.Fprintf(&frame, "\x1b[%dC", vl.CursorVisualCol)
		}
		os.Stdout.WriteString(frame.String())
	}

	e.stale = false
	e.renderedVisualRow = vl.CursorVisualRow
	e.renderedTotalRows = vl.TotalVisualRows
	e.renderedLine = vl.CurLine
	e.renderedLines = len(vl.Lines) - 1
}

// promptDisplayLine is a prompt line as Render draws it: a Tab (or any other control
// character) is drawn as one space. Written as is, a Tab would jump to the next tab stop,
// a distance that depends on the column, and the cursor would land inside the gap.
func promptDisplayLine(l string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, l)
}

func (e *PromptEditor) Clear() {
	if isTerminal(os.Stdout.Fd()) {
		if e.renderedVisualRow > 0 {
			fmt.Fprintf(os.Stdout, "\x1b[%dA", e.renderedVisualRow)
		}
		os.Stdout.WriteString("\r\x1b[J")
		os.Stdout.WriteString(markedPrompt(e.prompt, false))
	}

	e.runes = nil
	e.cursor = 0
	e.renderedVisualRow = 0
	e.renderedTotalRows = 0
	e.renderedLine = 0
	e.renderedLines = 0
}

func (e *PromptEditor) Insert(r rune) {
	e.insertWithoutRender(r)
	e.Render()
}

// insertWithoutRender inserts r like Insert but leaves the redraw to a later Render, so a
// burst of queued input (typed ahead, or pasted without bracketed paste) is drawn once
// instead of once per character.
func (e *PromptEditor) insertWithoutRender(r rune) {
	e.historyIndex = 0
	e.runes = slices.Insert(e.runes, e.cursor, r)
	e.cursor++
	e.stale = true
}

func (e *PromptEditor) InsertRunes(rs []rune) {
	if len(rs) == 0 {
		return
	}
	e.historyIndex = 0
	if e.cursor >= len(e.runes) {
		e.runes = append(e.runes, rs...)
		e.cursor = len(e.runes)
	} else {
		newRunes := make([]rune, 0, len(e.runes)+len(rs))
		newRunes = append(newRunes, e.runes[:e.cursor]...)
		newRunes = append(newRunes, rs...)
		newRunes = append(newRunes, e.runes[e.cursor:]...)
		e.runes = newRunes
		e.cursor += len(rs)
	}
	e.Render()
}

func (e *PromptEditor) Backspace() {
	if e.cursor <= 0 || len(e.runes) == 0 {
		return
	}
	e.historyIndex = 0
	e.runes = append(e.runes[:e.cursor-1], e.runes[e.cursor:]...)
	e.cursor--
	e.Render()
}

func (e *PromptEditor) Delete() {
	if e.cursor >= len(e.runes) {
		return
	}
	e.historyIndex = 0
	e.runes = append(e.runes[:e.cursor], e.runes[e.cursor+1:]...)
	e.Render()
}

func (e *PromptEditor) MoveLeft() {
	if e.cursor > 0 {
		e.cursor--
		e.Render()
	}
}

func (e *PromptEditor) MoveRight() {
	if e.cursor < len(e.runes) {
		e.cursor++
		e.Render()
	}
}

func (e *PromptEditor) MoveUp() {
	lines, curLine, curCol := e.Layout()
	if curLine > 0 {
		targetLine := curLine - 1
		targetCol := curCol
		lineLen := len([]rune(lines[targetLine]))
		if targetCol > lineLen {
			targetCol = lineLen
		}
		newPos := 0
		for _, line := range lines[:targetLine] {
			newPos += len([]rune(line)) + 1
		}
		newPos += targetCol
		e.cursor = newPos
		e.Render()
		return
	}

	// At top line: cycle backward through prompt history
	history := GetPromptHistory()
	if len(history) == 0 {
		return
	}
	if e.historyIndex == 0 {
		e.savedDraft = string(e.runes)
	}
	if e.historyIndex < len(history) {
		e.historyIndex++
		item := history[len(history)-e.historyIndex]
		e.runes = []rune(item)
		e.cursor = len(e.runes)
		e.Render()
	}
}

func (e *PromptEditor) MoveDown() {
	lines, curLine, curCol := e.Layout()
	if curLine < len(lines)-1 {
		targetLine := curLine + 1
		targetCol := curCol
		lineLen := len([]rune(lines[targetLine]))
		if targetCol > lineLen {
			targetCol = lineLen
		}
		newPos := 0
		for _, line := range lines[:targetLine] {
			newPos += len([]rune(line)) + 1
		}
		newPos += targetCol
		e.cursor = newPos
		e.Render()
		return
	}

	// At bottom line: cycle forward through prompt history
	if e.historyIndex <= 0 {
		return
	}
	e.historyIndex--
	if e.historyIndex == 0 {
		e.runes = []rune(e.savedDraft)
		e.cursor = len(e.runes)
		e.Render()
		return
	}
	history := GetPromptHistory()
	item := history[len(history)-e.historyIndex]
	e.runes = []rune(item)
	e.cursor = len(e.runes)
	e.Render()
}

func (e *PromptEditor) MoveHome() {
	lines, curLine, _ := e.Layout()
	newPos := 0
	for _, line := range lines[:curLine] {
		newPos += len([]rune(line)) + 1
	}
	e.cursor = newPos
	e.Render()
}

func (e *PromptEditor) MoveEnd() {
	lines, curLine, _ := e.Layout()
	newPos := 0
	for i, line := range lines[:curLine+1] {
		newPos += len([]rune(line))
		if i < curLine {
			newPos++
		}
	}
	e.cursor = newPos
	e.Render()
}

// lineStart returns the index of the first rune of the line holding the cursor.
func (e *PromptEditor) lineStart() int {
	i := e.cursor
	for i > 0 && e.runes[i-1] != '\n' {
		i--
	}
	return i
}

// lineEnd returns the index just past the last rune of the line holding the cursor.
func (e *PromptEditor) lineEnd() int {
	i := e.cursor
	for i < len(e.runes) && e.runes[i] != '\n' {
		i++
	}
	return i
}

// MoveLineStart moves the cursor to the start of the current line (Ctrl+A).
func (e *PromptEditor) MoveLineStart() {
	e.cursor = e.lineStart()
	e.Render()
}

// MoveLineEnd moves the cursor to the end of the current line (Ctrl+E).
func (e *PromptEditor) MoveLineEnd() {
	e.cursor = e.lineEnd()
	e.Render()
}

// deleteRange removes runes[start:end] and leaves the cursor at start.
func (e *PromptEditor) deleteRange(start, end int) {
	if start >= end {
		return
	}
	e.historyIndex = 0
	e.runes = slices.Delete(e.runes, start, end)
	e.cursor = start
	e.Render()
}

// DeleteToLineStart deletes from the start of the current line to the cursor (Ctrl+U).
func (e *PromptEditor) DeleteToLineStart() {
	e.deleteRange(e.lineStart(), e.cursor)
}

// DeleteToLineEnd deletes from the cursor to the end of the current line (Ctrl+K).
func (e *PromptEditor) DeleteToLineEnd() {
	e.deleteRange(e.cursor, e.lineEnd())
}

// DeleteWordBackward deletes trailing spaces and then the word before the
// cursor, never crossing a newline (Ctrl+W).
func (e *PromptEditor) DeleteWordBackward() {
	start := e.lineStart()
	i := e.cursor
	for i > start && unicode.IsSpace(e.runes[i-1]) {
		i--
	}
	for i > start && !unicode.IsSpace(e.runes[i-1]) {
		i--
	}
	e.deleteRange(i, e.cursor)
}

// isWordRune reports whether r belongs to a word for the Alt and Ctrl word keys: letters,
// digits and underscores, so "src/main.go" is three words, as in bash and zsh.
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// wordStartBefore returns the start of the word before pos, skipping the non-word runes
// in between. It does not go below limit.
func (e *PromptEditor) wordStartBefore(pos, limit int) int {
	for pos > limit && !isWordRune(e.runes[pos-1]) {
		pos--
	}
	for pos > limit && isWordRune(e.runes[pos-1]) {
		pos--
	}
	return pos
}

// wordEndAfter returns the end of the word after pos, skipping the non-word runes in
// between. It does not go above limit.
func (e *PromptEditor) wordEndAfter(pos, limit int) int {
	for pos < limit && !isWordRune(e.runes[pos]) {
		pos++
	}
	for pos < limit && isWordRune(e.runes[pos]) {
		pos++
	}
	return pos
}

// MoveWordLeft moves the cursor to the start of the previous word, across lines
// (Ctrl+Left, Alt+Left, Alt+B).
func (e *PromptEditor) MoveWordLeft() {
	e.cursor = e.wordStartBefore(e.cursor, 0)
	e.Render()
}

// MoveWordRight moves the cursor to the end of the next word, across lines
// (Ctrl+Right, Alt+Right, Alt+F).
func (e *PromptEditor) MoveWordRight() {
	e.cursor = e.wordEndAfter(e.cursor, len(e.runes))
	e.Render()
}

// DeleteWordBefore deletes the word before the cursor, stopping at punctuation and never
// crossing a newline (Alt+Backspace). Ctrl+W deletes back to whitespace instead.
func (e *PromptEditor) DeleteWordBefore() {
	e.deleteRange(e.wordStartBefore(e.cursor, e.lineStart()), e.cursor)
}

// DeleteWordAfter deletes the word after the cursor, never crossing a newline
// (Alt+D, Ctrl+Delete).
func (e *PromptEditor) DeleteWordAfter() {
	e.deleteRange(e.cursor, e.wordEndAfter(e.cursor, e.lineEnd()))
}
