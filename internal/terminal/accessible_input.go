package terminal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"hmj-agent/internal/terminal/theme"
)

// Accessibility input is append-only: it never moves the cursor, redraws a line or hides
// the cursor. It reads whole lines in the terminal's canonical (cooked) mode, so screen
// readers hear what was typed and the line editor works as usual.

// enterLineMode switches stdin to canonical mode with echo and returns a func that restores
// the previous settings. The key-driven editor and the halt listener leave stdin in raw mode,
// so accessibility input sets it explicitly. Signal handling (ISIG) is left as it is. When
// stdin is not a terminal it does nothing.
func enterLineMode() (restore func()) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(uintptr(fd)) {
		return func() {}
	}
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	cooked := *old
	cooked.Lflag |= unix.ICANON | unix.ECHO
	setTermios(fd, &cooked)
	return func() { setTermios(fd, old) }
}

// readAccessibleInput reads one prompt line by line from reader. A line ending in a backslash
// continues onto the next line, and the backslash is dropped. EOF (Ctrl+D) with nothing read
// returns io.EOF. Keyboard shortcuts are not available in this mode: their slash commands are.
// A lone "/" opens the command menu on a terminal.
func readAccessibleInput(reader *bufio.Reader) (string, ShortcutAction, error) {
	defer enterLineMode()()

	var sb strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			if sb.Len() == 0 {
				return "", ActionNone, err
			}
			break
		}
		text := strings.TrimRight(line, "\r\n")
		if err == nil && strings.HasSuffix(text, `\`) {
			sb.WriteString(strings.TrimSuffix(text, `\`))
			sb.WriteByte('\n')
			continue
		}
		sb.WriteString(text)
		break
	}

	input := strings.TrimSpace(dropControl(CleanInput(sb.String())))
	if input == "/" {
		if !isTerminal(os.Stdin.Fd()) {
			return "/shortcuts", ActionNone, nil
		}
		return accessibleCommandMenu(reader)
	}
	return input, ActionNone, nil
}

// dropControl removes control bytes (keeping newlines and tabs). In canonical mode a
// Ctrl+letter shortcut arrives as a literal control byte, which must not reach the prompt.
func dropControl(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// accessibleCommandMenu opens the slash-command list and returns the chosen command, or ""
// when the user cancels.
func accessibleCommandMenu(reader *bufio.Reader) (string, ShortcutAction, error) {
	item, confirmed, err := InteractiveSelect("slash commands", accessibleSlashCommands(), 0, reader, nil)
	if err != nil {
		return "", ActionNone, err
	}
	if confirmed && item.ID != "" {
		return item.ID, ActionNone, nil
	}
	return "", ActionNone, nil
}

// accessibleSlashCommands is DefaultSlashCommands without the Ctrl+key hints, which do not
// apply in accessibility mode.
func accessibleSlashCommands() []SelectItem {
	items := DefaultSlashCommands()
	for i, it := range items {
		if cut, _, found := strings.Cut(it.Text, " (Ctrl+"); found {
			items[i].Text = cut
		}
	}
	return items
}

// accessibleSelect is the screen-reader menu. It prints the items once as a numbered list and
// reads lines in canonical mode: a number selects, an empty line or "q" cancels, other text
// filters the list, and "r <n> <new name>" and "d <n>" rename or delete when the callbacks
// are given. The return values match the raw menu.
func accessibleSelect(title string, items []SelectItem, reader *bufio.Reader, cb *SelectCallbacks) (SelectItem, bool, error) {
	if reader == nil {
		reader = bufio.NewReader(os.Stdin)
	}
	defer enterLineMode()()

	// The key hints that close the raw menu's title, in parentheses, do not apply here; the
	// hint line below names the commands.
	if i := strings.LastIndex(title, " ("); i >= 0 && strings.HasSuffix(title, ")") {
		title = title[:i]
	}
	m := &accessibleMenu{items: slices.Clone(items), reader: reader, cb: cb}
	fmt.Println(theme.ASCII(title))
	fmt.Println(m.hint())
	return m.run()
}

// accessibleMenu is the state of one accessibleSelect call.
type accessibleMenu struct {
	items  []SelectItem // every item, kept current by rename and delete
	query  string       // the active text filter
	reader *bufio.Reader
	cb     *SelectCallbacks
}

// hint is the one-line summary of the menu commands.
func (m *accessibleMenu) hint() string {
	parts := []string{"number chooses", "text filters", "* shows all", "empty or q cancels"}
	if m.cb != nil && m.cb.OnRename != nil {
		parts = append(parts, "r <n> <new name> renames")
	}
	if m.cb != nil && m.cb.OnDelete != nil {
		parts = append(parts, "d <n> deletes")
	}
	return "Commands: " + strings.Join(parts, "; ") + "."
}

// shown returns the items that match the current filter.
func (m *accessibleMenu) shown() []SelectItem {
	return filterItems(m.items, m.query)
}

// print writes the shown items as a numbered list.
func (m *accessibleMenu) print() {
	shown := m.shown()
	if m.query != "" {
		fmt.Printf("filter: %s\n", m.query)
	}
	if len(shown) == 0 {
		fmt.Println("  (no matching results)")
	}
	for i, it := range shown {
		current := ""
		if it.IsActive {
			current = " (current)"
		}
		fmt.Printf("  %d. %s%s\n", i+1, menuLine(it.Text), current)
	}
}

// run prints the list and applies each line until the menu is answered.
func (m *accessibleMenu) run() (SelectItem, bool, error) {
	m.print()
	for {
		line, err := m.reader.ReadString('\n')
		if err != nil && line == "" {
			if errors.Is(err, io.EOF) {
				// Ctrl+D cancels the menu like an empty line. Reported as an error, it would
				// read as a failed command; if the input really ended, the prompt sees it next.
				return SelectItem{}, false, nil
			}
			return SelectItem{}, false, err
		}
		item, confirmed, done := m.apply(strings.TrimSpace(line))
		if done {
			return item, confirmed, nil
		}
		m.print()
	}
}

// apply handles one line. It reports done=true when the menu closes, with the chosen item
// and confirmed flag.
func (m *accessibleMenu) apply(line string) (SelectItem, bool, bool) {
	shown := m.shown()
	if line == "" || line == "q" {
		return SelectItem{}, false, true
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(shown) {
		return shown[n-1], true, true
	}
	if verb, n, rest, ok := parseAccessibleCommand(line); ok && m.supports(verb) {
		if n < 1 || n > len(shown) {
			fmt.Printf("no item %d\n", n)
			return SelectItem{}, false, false
		}
		if verb == "r" {
			m.rename(shown[n-1], rest)
			return SelectItem{}, false, false
		}
		return m.deleteItem(shown[n-1])
	}
	m.query = line
	if line == "*" {
		m.query = ""
	}
	return SelectItem{}, false, false
}

// supports reports whether the menu has a callback for the "r" or "d" command.
func (m *accessibleMenu) supports(verb string) bool {
	if m.cb == nil {
		return false
	}
	if verb == "r" {
		return m.cb.OnRename != nil
	}
	return m.cb.OnDelete != nil
}

// parseAccessibleCommand splits "r <n> <rest>" or "d <n>" into its verb, number and rest.
func parseAccessibleCommand(line string) (verb string, n int, rest string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || (fields[0] != "r" && fields[0] != "d") {
		return "", 0, "", false
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, "", false
	}
	return fields[0], n, strings.Join(fields[2:], " "), true
}

// rename applies a new name through the rename callback and reports the outcome.
func (m *accessibleMenu) rename(target SelectItem, newName string) {
	if newName == "" {
		fmt.Println("usage: r <n> <new name>")
		return
	}
	updatedText, err := m.cb.OnRename(target, newName)
	if err != nil {
		fmt.Println(theme.Danger("rename failed: " + err.Error()))
		return
	}
	if updatedText == "" {
		updatedText = newName
	}
	for i := range m.items {
		if m.items[i].ID == target.ID {
			m.items[i].Text = updatedText
		}
	}
	fmt.Println(theme.OK("renamed to " + newName))
}

// deleteItem asks for confirmation, then deletes through the callback. It reports done=true
// when no items remain, so the menu closes.
func (m *accessibleMenu) deleteItem(target SelectItem) (SelectItem, bool, bool) {
	fmt.Printf("delete \"%s\"? type y to confirm: ", menuLine(target.Text))
	// Input that ends before the line does (EOF) is no confirmation, even if it reads "y".
	answer, err := m.reader.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); err != nil || a != "y" && a != "yes" {
		fmt.Println("delete cancelled")
		return SelectItem{}, false, false
	}
	if err := m.cb.OnDelete(target); err != nil {
		fmt.Println(theme.Danger("delete failed: " + err.Error()))
		return SelectItem{}, false, false
	}
	m.items = slices.DeleteFunc(m.items, func(it SelectItem) bool { return it.ID == target.ID })
	fmt.Println(theme.OK("deleted " + menuLine(target.Text)))
	return SelectItem{}, false, len(m.items) == 0
}
