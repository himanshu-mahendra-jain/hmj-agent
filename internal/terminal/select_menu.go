package terminal

import (
	"bufio"
	"fmt"
	"hmj-agent/internal/terminal/theme"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// DefaultSlashCommands returns the list of interactive slash commands.
func DefaultSlashCommands() []SelectItem {
	return []SelectItem{
		{ID: "/new", Text: "/new        Start a new conversation (Ctrl+N)"},
		{ID: "/resume", Text: "/resume     Resume / manage conversations (Ctrl+R)"},
		{ID: "/models", Text: "/models     Switch AI model (Ctrl+O)"},
		{ID: "/jobs", Text: "/jobs       List background jobs and stop one"},
		{ID: "/health", Text: "/health     Check latency & health of all providers and models"},
		{ID: "/history", Text: "/history    Browse turns & copy prompt or AI output (Ctrl+Y)"},
		{ID: "/shortcuts", Text: "/shortcuts  Display shortcuts & commands cheatsheet table"},
	}
}

type SelectItem struct {
	ID       string
	Text     string
	IsActive bool
}

type SelectCallbacks struct {
	OnRename func(item SelectItem, newName string) (string, error)
	OnDelete func(item SelectItem) error
}

func filterItems(items []SelectItem, query string) []SelectItem {
	q := strings.TrimSpace(strings.ToLower(query))
	if q == "" {
		res := make([]SelectItem, len(items))
		copy(res, items)
		return res
	}
	var res []SelectItem
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Text), q) || (it.ID != "" && strings.Contains(strings.ToLower(it.ID), q)) {
			res = append(res, it)
		}
	}
	return res
}

// readConfirmRaw asks a yes/no question and answers it from the first key: y or Y confirms,
// any other key (Enter, n, Esc, an arrow, Ctrl+C, Ctrl+D) declines. Keys that arrive in the
// same burst, such as a double press or the rest of an arrow's escape sequence, are discarded
// so they can neither change the answer nor leak into the menu.
func readConfirmRaw(prompt string) bool {
	prompt = clipEnd(prompt, rowWidth()-len("  "+" [y/N]: ")-1)
	os.Stdout.WriteString(fmt.Sprintf("\r\033[2K  %s [y/N]: ", theme.Warn(prompt)))
	b := make([]byte, 1)
	n, err := os.Stdin.Read(b)
	confirmed := err == nil && n == 1 && (b[0] == 'y' || b[0] == 'Y')
	flushStdin()
	return confirmed
}

// readStringRaw reads a line of text: printable characters type, Backspace erases and Enter
// submits. Like readConfirmRaw, any other key (Esc, an arrow, Tab, Ctrl+C, Ctrl+D or another
// control key) cancels, and the rest of its burst, such as an arrow's escape sequence, is
// discarded so it cannot leak into the menu.
func readStringRaw(fd int, prompt string, initial string) (string, bool) {
	var val strings.Builder
	val.WriteString(initial)
	b := make([]byte, 1)
	for {
		cursor := " "
		if theme.Enabled() {
			cursor = theme.Mark(" ")
		}
		shown := clipStart(val.String(), rowWidth()-len("  ")-VisibleWidth(prompt)-1)
		os.Stdout.WriteString(fmt.Sprintf("\r\033[2K  %s%s%s", theme.Info(prompt), shown, cursor))
		n, err := os.Stdin.Read(b)
		if err != nil || n == 0 {
			return "", false
		}
		switch ch := b[0]; {
		case ch == '\r' || ch == '\n':
			flushStdin()
			return strings.TrimSpace(val.String()), true
		case ch == 0x7f || ch == 0x08: // Backspace
			s := val.String()
			if len(s) > 0 {
				val.Reset()
				val.WriteString(s[:len(s)-1])
			}
		case ch >= 32 && ch <= 126:
			val.WriteByte(ch)
		case ch >= 0x80: // Bytes of a non-ASCII character are not typed, but do not cancel
		default: // Esc, an arrow, Tab, Ctrl+C, Ctrl+D or another control key cancels
			flushStdin()
			return "", false
		}
	}
}

// InteractiveSelect displays a searchable, scrollable menu in terminal raw mode. In
// accessibility mode it prints a numbered list and reads a line instead (see accessibleSelect).
// Arrow keys (Up/Down) navigate, printable chars filter the list in real-time.
// Enter confirms, Esc / Ctrl+C cancels.
// When cb is provided, Ctrl+E renames and Ctrl+D / Delete deletes items.
// Keys typed before the menu opened are discarded.
// Returns (selectedItem, confirmed, error).
func InteractiveSelect(
	title string,
	items []SelectItem,
	defaultIdx int,
	reader *bufio.Reader,
	cb *SelectCallbacks,
) (SelectItem, bool, error) {
	return interactiveSelect(title, items, defaultIdx, reader, cb, true)
}

// interactiveSelect is InteractiveSelect. flush discards keys typed before the menu opened,
// such as stray keys pressed while the model was working. The slash menu passes false: the
// keys after its "/" are the rest of a command typed faster than the menu could draw.
func interactiveSelect(
	title string,
	items []SelectItem,
	defaultIdx int,
	reader *bufio.Reader,
	cb *SelectCallbacks,
	flush bool,
) (SelectItem, bool, error) {
	if len(items) == 0 {
		return SelectItem{}, false, nil
	}
	if theme.Accessibility() {
		return accessibleSelect(title, items, reader, cb)
	}

	fd := int(os.Stdin.Fd())
	if !isTerminal(uintptr(fd)) {
		return selectWithoutTerminal(items, reader)
	}

	oldTermios, termErr := unix.IoctlGetTermios(fd, unix.TCGETS)
	if termErr != nil {
		return SelectItem{}, false, fmt.Errorf("menu unavailable: %w", termErr)
	}

	if flush {
		flushStdin()
	}

	raw := *oldTermios
	raw.Lflag &^= (unix.ICANON | unix.ECHO)
	raw.Iflag &^= unix.IXON // Ctrl+S is ignored, as at the prompt, instead of freezing output
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	// Deliver Ctrl+C as a key (handled in-app) instead of raising SIGINT, which
	// would quit the agent. Other signal keys such as Ctrl+Z keep working.
	raw.Cc[unix.VINTR] = 0

	setTermios(fd, &raw)
	defer func() {
		setTermios(fd, oldTermios)
	}()

	os.Stdout.WriteString("\x1b[?25l")       // Hide cursor
	defer os.Stdout.WriteString("\x1b[?25h") // Show cursor

	m := newSelectMenu(title, fd, items, defaultIdx, cb)
	m.render(true)
	return m.run()
}

// Number of item rows visible in the InteractiveSelect menu at once.
const selectWindowSize = 7

// selectMenuLines is the menu height: title, search line, item window and footer.
const selectMenuLines = 1 + 1 + selectWindowSize + 1

// selectMenu holds the state of an InteractiveSelect menu while it is on screen.
type selectMenu struct {
	title       string
	fd          int
	items       []SelectItem
	filtered    []SelectItem
	query       string
	selected    int
	windowStart int
	flashMsg    string
	cb          *SelectCallbacks
}

// newSelectMenu builds the menu state, highlighting defaultIdx when it is in range.
func newSelectMenu(title string, fd int, items []SelectItem, defaultIdx int, cb *SelectCallbacks) *selectMenu {
	filtered := filterItems(items, "")
	selected := defaultIdx
	if selected < 0 || selected >= len(filtered) {
		selected = 0
	}
	return &selectMenu{title: title, fd: fd, items: items, filtered: filtered, selected: selected, cb: cb}
}

// run reads keys until the menu is confirmed or cancelled, clearing the menu before it returns.
func (m *selectMenu) run() (SelectItem, bool, error) {
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if err != nil || n == 0 {
			m.clear()
			return SelectItem{}, false, err
		}
		if item, confirmed, done := m.handleKey(b[0]); done {
			m.clear()
			return item, confirmed, nil
		}
	}
}

// handleKey applies one keypress. It returns done=true when the menu should close,
// along with the item and confirmed flag describing the outcome.
func (m *selectMenu) handleKey(ch byte) (SelectItem, bool, bool) {
	switch {
	case ch == 0x1b:
		return m.handleEscape()
	case ch == '\t': // Tab also moves down
		m.move(true)
	case ch == '\r' || ch == '\n': // Enter confirms the selection
		if len(m.filtered) == 0 {
			return SelectItem{}, false, false
		}
		return m.filtered[m.selected], true, true
	case ch == 0x03: // Ctrl+C cancels
		return SelectItem{}, false, true
	case ch == 0x04: // Ctrl+D deletes when callbacks are present
		return SelectItem{}, false, m.deleteHighlighted()
	case ch == 0x05: // Ctrl+E renames when callbacks are present
		m.renameHighlighted()
	case ch == 0x7f || ch == 0x08: // Backspace
		m.flashMsg = ""
		if len(m.query) > 0 {
			m.setQuery(m.query[:len(m.query)-1])
			m.render(false)
		}
	case ch == 0x15 || ch == 0x17: // Ctrl+U or Ctrl+W clears the search query
		m.flashMsg = ""
		m.setQuery("")
		m.render(false)
	case ch >= 32 && ch <= 126: // Printable ASCII extends the filter query
		m.flashMsg = ""
		m.setQuery(m.query + string(ch))
		m.render(false)
	}
	return SelectItem{}, false, false
}

// handleEscape reads the remainder of an escape sequence and applies arrow keys,
// the Delete key, or a standalone Esc press.
func (m *selectMenu) handleEscape() (SelectItem, bool, bool) {
	switch seq := readEscapeSeq(m.fd); seq {
	case "": // Standalone Esc cancels
		return SelectItem{}, false, true
	case "[A", "OA": // Arrow Up
		m.move(false)
	case "[B", "OB": // Arrow Down
		m.move(true)
	case "[3~": // Delete key
		return SelectItem{}, false, m.deleteHighlighted()
	}
	return SelectItem{}, false, false
}

// move shifts the highlight down or up, wrapping at either end, and redraws the menu.
func (m *selectMenu) move(down bool) {
	m.flashMsg = ""
	if len(m.filtered) == 0 {
		return
	}
	if down {
		if m.selected < len(m.filtered)-1 {
			m.selected++
		} else {
			m.selected = 0
		}
	} else {
		if m.selected > 0 {
			m.selected--
		} else {
			m.selected = len(m.filtered) - 1
		}
	}
	m.render(false)
}

// setQuery replaces the search query, refilters the items and resets the highlight to the top.
func (m *selectMenu) setQuery(query string) {
	m.query = query
	m.filtered = filterItems(m.items, query)
	m.selected = 0
	m.windowStart = 0
}

// deleteHighlighted deletes the highlighted item after confirmation and redraws the menu.
// It reports true when no items remain, in which case the caller should close the menu.
func (m *selectMenu) deleteHighlighted() bool {
	if m.cb == nil || m.cb.OnDelete == nil || len(m.filtered) == 0 {
		return false
	}
	target := m.filtered[m.selected]
	if !readConfirmRaw(fmt.Sprintf("delete conversation \"%s\"?", menuLine(target.Text))) {
		m.flashMsg = "delete cancelled"
	} else if err := m.cb.OnDelete(target); err != nil {
		m.flashMsg = menuLine(fmt.Sprintf("error deleting: %v", err))
	} else {
		m.flashMsg = "deleted conversation"
		m.removeItem(target.ID)
		if len(m.items) == 0 {
			return true
		}
	}
	m.render(false)
	return false
}

// removeItem drops the item with the given ID and refilters, keeping the highlight in range.
func (m *selectMenu) removeItem(id string) {
	var remaining []SelectItem
	for _, it := range m.items {
		if it.ID != id {
			remaining = append(remaining, it)
		}
	}
	m.items = remaining
	m.filtered = filterItems(m.items, m.query)
	if m.selected >= len(m.filtered) {
		m.selected = len(m.filtered) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
}

// renameHighlighted prompts for a new name for the highlighted item, applies it through
// the rename callback and redraws the menu. As with delete, a key other than text, Backspace
// or Enter cancels the prompt, and so does submitting a blank name.
func (m *selectMenu) renameHighlighted() {
	if m.cb == nil || m.cb.OnRename == nil || len(m.filtered) == 0 {
		return
	}
	target := m.filtered[m.selected]
	newName, ok := readStringRaw(m.fd, "rename to: ", "")
	if !ok || newName == "" {
		m.flashMsg = "rename cancelled"
	} else if updatedText, err := m.cb.OnRename(target, newName); err != nil {
		m.flashMsg = menuLine(fmt.Sprintf("error renaming: %v", err))
	} else {
		for i := range m.items {
			if m.items[i].ID == target.ID {
				if updatedText != "" {
					m.items[i].Text = updatedText
				} else {
					m.items[i].Text = newName
				}
			}
		}
		m.filtered = filterItems(m.items, m.query)
		m.flashMsg = fmt.Sprintf("renamed to \"%s\"", newName)
	}
	m.render(false)
}

// clear erases the menu from the screen.
func (m *selectMenu) clear() {
	os.Stdout.WriteString(fmt.Sprintf("\033[%dA\033[0J", selectMenuLines))
}

// render draws the menu. After the first draw it moves the cursor back over the previous
// frame so the menu is redrawn in place.
func (m *selectMenu) render(firstTime bool) {
	m.scrollToSelection()

	var sb strings.Builder
	if !firstTime {
		sb.WriteString(fmt.Sprintf("\033[%dA", selectMenuLines))
	}

	// Line 1: Title & help
	width := rowWidth()
	sb.WriteString(fmt.Sprintf("\r\033[2K%s\n", theme.Muted(clipEnd(m.title, width))))

	// Line 2: Search line
	cursor := " "
	if theme.Enabled() {
		cursor = theme.Mark(" ")
	}
	query := clipStart(m.query, width-len("  > ")-1)
	sb.WriteString(fmt.Sprintf("\r\033[2K  %s %s%s\n", theme.Prompt(">"), query, cursor))

	// Lines 3 to (3 + selectWindowSize - 1): Items window
	m.writeItemWindow(&sb, width)

	// Last line: Footer
	m.writeFooter(&sb, width)

	// The cursor rests on the line below the footer, where the delete confirmation and the
	// rename prompt are drawn. Erase it so an answered prompt does not stay on screen.
	sb.WriteString("\r\033[2K")

	os.Stdout.WriteString(sb.String())
}

// scrollToSelection moves the visible window so the highlighted item is on screen.
func (m *selectMenu) scrollToSelection() {
	if m.selected < m.windowStart {
		m.windowStart = m.selected
	}
	if m.selected >= m.windowStart+selectWindowSize {
		m.windowStart = m.selected - selectWindowSize + 1
	}
	if m.windowStart < 0 {
		m.windowStart = 0
	}
}

// writeItemWindow writes the rows of the visible item window, padding unused rows with blanks.
func (m *selectMenu) writeItemWindow(sb *strings.Builder, width int) {
	for row := range selectWindowSize {
		itemIdx := m.windowStart + row
		if itemIdx < len(m.filtered) {
			it := m.filtered[itemIdx]
			marker := " "
			if it.IsActive {
				marker = "*"
			}
			label := fmt.Sprintf("%s %2d. ", marker, itemIdx+1)
			text := clipEnd(menuLine(it.Text), width-len("   ")-len(label))
			if itemIdx == m.selected {
				selGlyph := "❯"
				if theme.Accessibility() {
					selGlyph = ">"
				}
				sb.WriteString(fmt.Sprintf("\r\033[2K %s %s\n", theme.Accent(selGlyph), theme.Bold(label+text)))
			} else {
				sb.WriteString(fmt.Sprintf("\r\033[2K   %s%s\n", label, text))
			}
		} else if row == 0 && len(m.filtered) == 0 {
			sb.WriteString(fmt.Sprintf("\r\033[2K    %s\n", theme.Muted("(no matching results)")))
		} else {
			sb.WriteString("\r\033[2K\n")
		}
	}
}

// menuLine is item text as one row of a menu shows it. Item text can come from outside
// the program (a conversation name edited into the session file or written by a model), so
// escape sequences and other control characters are removed, and a Tab or newline, which
// would break the row and throw off the in-place redraw, becomes a space.
func menuLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return ' '
		}
		return r
	}, theme.Sanitize(s))
}

// writeFooter writes the status line: a flash message, or the match count with scroll badges.
func (m *selectMenu) writeFooter(sb *strings.Builder, width int) {
	sb.WriteString("\r\033[2K")
	switch {
	case m.flashMsg != "":
		sb.WriteString(fmt.Sprintf("  %s", theme.OK(clipEnd(m.flashMsg, width-2-VisibleWidth(theme.OK(""))))))
	case len(m.filtered) == 0:
		sb.WriteString(fmt.Sprintf("  %s", theme.Muted("0 matches")))
	default:
		footerText := fmt.Sprintf("%d/%d matches", m.selected+1, len(m.filtered))
		upGlyph, downGlyph := "↑", "↓"
		if theme.Accessibility() {
			upGlyph, downGlyph = "^", "v"
		}
		var badges []string
		if m.windowStart > 0 {
			badges = append(badges, fmt.Sprintf("%s %d more", upGlyph, m.windowStart))
		}
		remainingBelow := len(m.filtered) - (m.windowStart + selectWindowSize)
		if remainingBelow > 0 {
			badges = append(badges, fmt.Sprintf("%s %d more", downGlyph, remainingBelow))
		}
		if len(badges) > 0 {
			footerText += fmt.Sprintf(" [%s]", strings.Join(badges, ", "))
		}
		sb.WriteString(fmt.Sprintf("  %s", theme.Muted(clipEnd(footerText, width-2))))
	}
	sb.WriteString("\n")
}

// selectWithoutTerminal picks an item when stdin is not a terminal. It reads one line from
// reader and matches it as a 1-based number, an ID or a case-insensitive text fragment.
// End of input, an empty line or text matching no item cancels the menu.
func selectWithoutTerminal(items []SelectItem, reader *bufio.Reader) (SelectItem, bool, error) {
	if reader != nil {
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			return SelectItem{}, false, nil
		}
		choice := strings.TrimSpace(line)
		if choice == "" {
			return SelectItem{}, false, nil
		}
		// The whole answer must be a number: Sscanf would also read "2fast/gpt" as item 2.
		if idx, err := strconv.Atoi(choice); err == nil && idx >= 1 && idx <= len(items) {
			return items[idx-1], true, nil
		}
		for _, it := range items {
			if strings.EqualFold(it.ID, choice) || strings.Contains(strings.ToLower(it.Text), strings.ToLower(choice)) {
				return it, true, nil
			}
		}
	}
	return SelectItem{}, false, nil
}
