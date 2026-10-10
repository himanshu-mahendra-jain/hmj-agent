package terminal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"hmj-agent/internal/terminal/theme"
)

// menuEnv replaces stdin with a pipe the test feeds and stdout with a file the test reads,
// so selectMenu key handling can be driven one step at a time.
type menuEnv struct {
	t   *testing.T
	in  *os.File
	out *os.File
}

func newMenuEnv(t *testing.T) *menuEnv {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = r, out
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		r.Close()
		w.Close()
		out.Close()
	})
	return &menuEnv{t: t, in: w, out: out}
}

// feed queues bytes on stdin.
func (e *menuEnv) feed(s string) {
	e.t.Helper()
	if _, err := e.in.WriteString(s); err != nil {
		e.t.Fatal(err)
	}
}

// fdIn is the descriptor of the stdin pipe.
func (e *menuEnv) fdIn() int { return int(os.Stdin.Fd()) }

// closeInput signals end of input.
func (e *menuEnv) closeInput() { e.in.Close() }

// output returns everything written to stdout since the last call.
func (e *menuEnv) output() string {
	e.t.Helper()
	b, err := os.ReadFile(e.out.Name())
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.out.Truncate(0); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.out.Seek(0, io.SeekStart); err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func numberedItems(n int) []SelectItem {
	items := make([]SelectItem, n)
	for i := range items {
		items[i] = SelectItem{ID: fmt.Sprintf("id%d", i+1), Text: fmt.Sprintf("item %d", i+1)}
	}
	return items
}

func newTestMenu(env *menuEnv, items []SelectItem, def int, cb *SelectCallbacks) *selectMenu {
	return newSelectMenu("pick one", int(os.Stdin.Fd()), items, def, cb)
}

func TestNewSelectMenuDefaultIndex(t *testing.T) {
	items := numberedItems(3)
	for _, tc := range []struct{ def, want int }{{1, 1}, {2, 2}, {3, 0}, {-1, 0}, {0, 0}} {
		m := newSelectMenu("t", 0, items, tc.def, nil)
		if m.selected != tc.want {
			t.Errorf("defaultIdx %d: selected = %d, want %d", tc.def, m.selected, tc.want)
		}
		if len(m.filtered) != 3 {
			t.Errorf("filtered = %d items, want all 3", len(m.filtered))
		}
	}
}

func TestSelectMenuMoveWrapsAndScrolls(t *testing.T) {
	env := newMenuEnv(t)
	m := newTestMenu(env, numberedItems(10), 0, nil)
	m.render(true)
	env.output()

	// Walk down past the 7-row window: the window follows the highlight.
	for range 7 {
		m.move(true)
	}
	if m.selected != 7 || m.windowStart != 1 {
		t.Fatalf("after 7 downs selected=%d windowStart=%d, want 7 and 1", m.selected, m.windowStart)
	}
	out := env.output()
	if !strings.Contains(out, "8/10 matches [↑ 1 more, ↓ 2 more]") {
		t.Errorf("footer missing scroll badges: %q", out)
	}

	// Down from the last item wraps to the first and scrolls back to the top.
	m.move(true)
	m.move(true)
	m.move(true)
	if m.selected != 0 || m.windowStart != 0 {
		t.Fatalf("after wrap selected=%d windowStart=%d, want 0 and 0", m.selected, m.windowStart)
	}
	if out := env.output(); !strings.Contains(out, "1/10 matches [↓ 3 more]") {
		t.Errorf("footer after wrap = %q", out)
	}

	// Up from the first item wraps to the last.
	m.move(false)
	if m.selected != 9 || m.windowStart != 3 {
		t.Fatalf("up from top selected=%d windowStart=%d, want 9 and 3", m.selected, m.windowStart)
	}
	if out := env.output(); !strings.Contains(out, "10/10 matches [↑ 3 more]") {
		t.Errorf("footer at bottom = %q", out)
	}
}

func TestSelectMenuArrowKeysAndTab(t *testing.T) {
	env := newMenuEnv(t)
	m := newTestMenu(env, numberedItems(4), 0, nil)
	env.feed("[B")
	m.handleKey(0x1b)
	if m.selected != 1 {
		t.Fatalf("Down arrow: selected = %d, want 1", m.selected)
	}
	env.feed("[B")
	m.handleKey(0x1b)
	env.feed("[A")
	m.handleKey(0x1b)
	if m.selected != 1 {
		t.Fatalf("Up arrow: selected = %d, want 1", m.selected)
	}
	m.handleKey(0x7f)
	m.move(false)
	m.handleKey('\t')
	if m.selected != 1 {
		t.Fatalf("Tab: selected = %d, want 1", m.selected)
	}
	// An unknown escape sequence (Left arrow) neither moves nor closes the menu.
	env.feed("[D")
	if _, _, done := m.handleKey(0x1b); done || m.selected != 1 {
		t.Fatalf("unknown sequence changed state: done=%v selected=%d", done, m.selected)
	}
}

// readEscapeSeq stops at the first letter, so the SS3 form of the arrow keys ("ESC O B",
// sent in application cursor mode) is cut after the "O" and the "B" is typed into the filter.
func TestSelectMenuSS3ArrowKeys(t *testing.T) {
	env := newMenuEnv(t)
	m := newTestMenu(env, numberedItems(4), 0, nil)
	env.feed("OB")
	m.handleKey(0x1b)
	if m.selected != 1 || m.query != "" {
		t.Fatalf("SS3 Down: selected=%d query=%q, want 1 and empty", m.selected, m.query)
	}
	env.feed("OA")
	m.handleKey(0x1b)
	if m.selected != 0 || m.query != "" {
		t.Fatalf("SS3 Up: selected=%d query=%q, want 0 and empty", m.selected, m.query)
	}
}

func TestSelectMenuTypeToFilterAndBackspace(t *testing.T) {
	env := newMenuEnv(t)
	items := []SelectItem{
		{ID: "1", Text: "alpha"}, {ID: "2", Text: "beta"}, {ID: "3", Text: "alphabet"}, {ID: "4", Text: "gamma"},
	}
	m := newTestMenu(env, items, 2, nil)

	for _, ch := range "alp" {
		m.handleKey(byte(ch))
	}
	if m.query != "alp" || len(m.filtered) != 2 || m.filtered[1].Text != "alphabet" {
		t.Fatalf("filter 'alp': query=%q filtered=%v", m.query, m.filtered)
	}
	if m.selected != 0 {
		t.Errorf("typing resets highlight to top, got %d", m.selected)
	}
	if out := env.output(); !strings.Contains(out, "> alp") || !strings.Contains(out, "1/2 matches") {
		t.Errorf("render after typing = %q", out)
	}

	m.handleKey(0x7f) // Backspace
	if m.query != "al" {
		t.Errorf("query after backspace = %q, want al", m.query)
	}
	m.handleKey(0x08)
	m.handleKey(0x08)
	m.handleKey(0x08) // backspace on empty query is a no-op
	if m.query != "" || len(m.filtered) != 4 {
		t.Errorf("query=%q filtered=%d, want empty and 4", m.query, len(m.filtered))
	}

	m.handleKey('g')
	m.handleKey(0x15) // Ctrl+U clears
	if m.query != "" || len(m.filtered) != 4 {
		t.Errorf("Ctrl+U: query=%q filtered=%d", m.query, len(m.filtered))
	}
	m.handleKey('g')
	m.handleKey(0x17) // Ctrl+W clears
	if m.query != "" {
		t.Errorf("Ctrl+W: query=%q", m.query)
	}
}

func TestSelectMenuEmptyFilterResult(t *testing.T) {
	env := newMenuEnv(t)
	m := newTestMenu(env, numberedItems(3), 0, nil)
	m.handleKey('z')
	if len(m.filtered) != 0 {
		t.Fatalf("filtered = %v, want none", m.filtered)
	}
	out := env.output()
	if !strings.Contains(out, "(no matching results)") || !strings.Contains(out, "0 matches") {
		t.Errorf("empty render = %q", out)
	}
	// Enter with nothing to pick keeps the menu open; arrows are inert.
	if _, confirmed, done := m.handleKey('\r'); confirmed || done {
		t.Errorf("Enter on empty list: confirmed=%v done=%v, want both false", confirmed, done)
	}
	m.move(true)
	m.move(false)
	if m.selected != 0 {
		t.Errorf("selected = %d after moving in an empty list", m.selected)
	}
	// Backspace restores the full list.
	m.handleKey(0x7f)
	if len(m.filtered) != 3 {
		t.Errorf("filtered after backspace = %d, want 3", len(m.filtered))
	}
}

func TestSelectMenuEnterSelectsHighlighted(t *testing.T) {
	env := newMenuEnv(t)
	m := newTestMenu(env, numberedItems(5), 0, nil)
	m.handleKey('\t')
	m.handleKey('\t')
	item, confirmed, done := m.handleKey('\n')
	if !done || !confirmed || item.ID != "id3" {
		t.Fatalf("Enter = (%v, %v, %v), want id3 confirmed and done", item, confirmed, done)
	}
}

func TestSelectMenuCancel(t *testing.T) {
	env := newMenuEnv(t)
	m := newTestMenu(env, numberedItems(3), 0, nil)
	if item, confirmed, done := m.handleKey(0x03); !done || confirmed || item.ID != "" {
		t.Errorf("Ctrl+C = (%v, %v, %v)", item, confirmed, done)
	}
	env.closeInput() // a bare Esc: nothing follows it
	if item, confirmed, done := m.handleKey(0x1b); !done || confirmed || item.ID != "" {
		t.Errorf("Esc = (%v, %v, %v)", item, confirmed, done)
	}
}

func TestSelectMenuRunSequences(t *testing.T) {
	t.Run("filter then Enter", func(t *testing.T) {
		env := newMenuEnv(t)
		env.feed("item 3\r")
		env.closeInput()
		m := newTestMenu(env, numberedItems(5), 0, nil)
		item, confirmed, err := m.run()
		if err != nil || !confirmed || item.ID != "id3" {
			t.Fatalf("run = (%v, %v, %v), want id3 confirmed", item, confirmed, err)
		}
		if out := env.output(); !strings.HasSuffix(out, "\033[10A\033[0J") {
			t.Errorf("menu not cleared on exit, output tail %q", out[max(0, len(out)-20):])
		}
	})
	t.Run("Esc cancels", func(t *testing.T) {
		env := newMenuEnv(t)
		env.feed("\x1b")
		env.closeInput()
		_, confirmed, err := newTestMenu(env, numberedItems(2), 0, nil).run()
		if confirmed || err != nil {
			t.Fatalf("run = (%v, %v), want cancelled without error", confirmed, err)
		}
	})
	t.Run("end of input is an error", func(t *testing.T) {
		env := newMenuEnv(t)
		env.closeInput()
		_, confirmed, err := newTestMenu(env, numberedItems(2), 0, nil).run()
		if confirmed || !errors.Is(err, io.EOF) {
			t.Fatalf("run = (%v, %v), want EOF", confirmed, err)
		}
	})
	t.Run("arrow down then Enter", func(t *testing.T) {
		env := newMenuEnv(t)
		env.feed("\x1b[B\x1b[B\r")
		env.closeInput()
		item, confirmed, err := newTestMenu(env, numberedItems(4), 0, nil).run()
		if err != nil || !confirmed || item.ID != "id3" {
			t.Fatalf("run = (%v, %v, %v)", item, confirmed, err)
		}
	})
}

func TestSelectMenuRename(t *testing.T) {
	// newMenu builds a menu on its own item slice: renames edit the slice in place.
	newMenu := func(env *menuEnv, def int, cb *SelectCallbacks) *selectMenu {
		return newTestMenu(env, []SelectItem{{ID: "a", Text: "first"}, {ID: "b", Text: "second"}}, def, cb)
	}

	t.Run("applies callback text", func(t *testing.T) {
		env := newMenuEnv(t)
		var gotItem SelectItem
		var gotName string
		m := newMenu(env, 1, &SelectCallbacks{
			OnRename: func(it SelectItem, name string) (string, error) {
				gotItem, gotName = it, name
				return "Second (renamed)", nil
			},
		})
		env.feed("new name\r")
		m.handleKey(0x05)
		if gotItem.ID != "b" || gotName != "new name" {
			t.Fatalf("callback got (%v, %q)", gotItem, gotName)
		}
		if m.items[1].Text != "Second (renamed)" || m.filtered[1].Text != "Second (renamed)" || m.items[0].Text != "first" {
			t.Errorf("items after rename: %v / %v", m.items, m.filtered)
		}
		if out := env.output(); !strings.Contains(out, `renamed to "new name"`) {
			t.Errorf("footer missing flash: %q", out)
		}
	})
	t.Run("empty callback text falls back to typed name", func(t *testing.T) {
		env := newMenuEnv(t)
		m := newMenu(env, 0, &SelectCallbacks{
			OnRename: func(SelectItem, string) (string, error) { return "", nil },
		})
		env.feed("typed\r")
		m.handleKey(0x05)
		if m.items[0].Text != "typed" {
			t.Errorf("text = %q, want typed", m.items[0].Text)
		}
	})
	// Like the delete prompt, any key other than text, Backspace or Enter cancels the rename,
	// and so does a blank name. The cancelling key's burst must not reach the menu: an arrow
	// that cancels the rename does not also move the highlight.
	for _, tc := range []struct{ name, input string }{
		{"Esc", "abc\x1b"},
		{"Up arrow", "abc\x1b[A"},
		{"Down arrow", "abc\x1b[B"},
		{"Ctrl+C", "abc\x03"},
		{"Ctrl+D", "abc\x04"},
		{"Tab", "abc\t"},
		{"blank name", "   \r"},
	} {
		t.Run(tc.name+" cancels without calling back", func(t *testing.T) {
			env := newMenuEnv(t)
			called := false
			m := newMenu(env, 1, &SelectCallbacks{
				OnRename: func(SelectItem, string) (string, error) { called = true; return "", nil },
			})
			env.feed(tc.input)
			if _, _, done := m.handleKey(0x05); done {
				t.Fatal("cancelling a rename closed the menu")
			}
			if called || m.items[1].Text != "second" {
				t.Errorf("cancelled rename changed state: called=%v items=%v", called, m.items)
			}
			if m.selected != 1 {
				t.Errorf("selected = %d, want 1: the cancelling key moved the highlight", m.selected)
			}
			if out := env.output(); !strings.Contains(out, "rename cancelled") {
				t.Errorf("footer missing cancel flash: %q", out)
			}
			if pending, _ := hasInputPending(os.Stdin.Fd(), 20*time.Millisecond); pending {
				t.Error("input left over after the cancelled rename")
			}
		})
	}
	t.Run("callback error leaves text unchanged", func(t *testing.T) {
		env := newMenuEnv(t)
		m := newMenu(env, 0, &SelectCallbacks{
			OnRename: func(SelectItem, string) (string, error) { return "", errors.New("boom") },
		})
		env.feed("x\r")
		m.handleKey(0x05)
		if m.items[0].Text != "first" || m.flashMsg != "error renaming: boom" {
			t.Errorf("text=%q flash=%q after failed rename", m.items[0].Text, m.flashMsg)
		}
	})
	t.Run("without callbacks Ctrl+E does nothing", func(t *testing.T) {
		env := newMenuEnv(t)
		m := newMenu(env, 0, nil)
		env.feed("ignored\r")
		m.handleKey(0x05)
		if m.items[0].Text != "first" || env.output() != "" {
			t.Error("Ctrl+E without callbacks should not read input or draw")
		}
	})
}

func TestSelectMenuDelete(t *testing.T) {
	newItems := func() []SelectItem {
		return []SelectItem{{ID: "a", Text: "one"}, {ID: "b", Text: "two"}, {ID: "c", Text: "three"}}
	}

	t.Run("confirmed delete removes the item", func(t *testing.T) {
		env := newMenuEnv(t)
		var deleted []string
		m := newTestMenu(env, newItems(), 2, &SelectCallbacks{
			OnDelete: func(it SelectItem) error { deleted = append(deleted, it.ID); return nil },
		})
		env.feed("y\r")
		if _, _, done := m.handleKey(0x04); done {
			t.Fatal("menu closed although items remain")
		}
		if len(deleted) != 1 || deleted[0] != "c" {
			t.Fatalf("deleted = %v, want [c]", deleted)
		}
		if len(m.items) != 2 || len(m.filtered) != 2 || m.selected != 1 {
			t.Errorf("items=%v selected=%d, want 2 items with highlight clamped to 1", m.items, m.selected)
		}
		out := env.output()
		if !strings.Contains(out, `delete conversation "three"?`) || !strings.Contains(out, "deleted conversation") {
			t.Errorf("output missing prompt/flash: %q", out)
		}
	})
	t.Run("Delete key sequence", func(t *testing.T) {
		env := newMenuEnv(t)
		var deleted []string
		m := newTestMenu(env, newItems(), 0, &SelectCallbacks{
			OnDelete: func(it SelectItem) error { deleted = append(deleted, it.ID); return nil },
		})
		env.feed("[3~y\r")
		m.handleKey(0x1b)
		if len(deleted) != 1 || deleted[0] != "a" || len(m.items) != 2 {
			t.Errorf("deleted=%v items=%v", deleted, m.items)
		}
	})
	t.Run("declined delete keeps the item", func(t *testing.T) {
		env := newMenuEnv(t)
		called := false
		m := newTestMenu(env, newItems(), 0, &SelectCallbacks{
			OnDelete: func(SelectItem) error { called = true; return nil },
		})
		env.feed("n\r")
		m.handleKey(0x04)
		if called || len(m.items) != 3 {
			t.Errorf("declined delete: called=%v items=%d", called, len(m.items))
		}
	})
	t.Run("callback error shows message and keeps item", func(t *testing.T) {
		env := newMenuEnv(t)
		m := newTestMenu(env, newItems(), 0, &SelectCallbacks{
			OnDelete: func(SelectItem) error { return errors.New("disk full") },
		})
		env.feed("y\r")
		m.handleKey(0x04)
		if len(m.items) != 3 {
			t.Errorf("items = %d, want 3", len(m.items))
		}
		if out := env.output(); !strings.Contains(out, "error deleting: disk full") {
			t.Errorf("output = %q", out)
		}
	})
	t.Run("deleting the last item closes the menu", func(t *testing.T) {
		env := newMenuEnv(t)
		m := newTestMenu(env, newItems()[:1], 0, &SelectCallbacks{
			OnDelete: func(SelectItem) error { return nil },
		})
		env.feed("y\r")
		item, confirmed, done := m.handleKey(0x04)
		if !done || confirmed || item.ID != "" {
			t.Errorf("handleKey = (%v, %v, %v), want done, not confirmed", item, confirmed, done)
		}
	})
	t.Run("without callbacks Ctrl+D is inert", func(t *testing.T) {
		env := newMenuEnv(t)
		m := newTestMenu(env, newItems(), 0, nil)
		if _, _, done := m.handleKey(0x04); done || len(m.items) != 3 {
			t.Error("Ctrl+D without callbacks changed state")
		}
	})
}

func TestSelectMenuRemoveItemKeepsHighlightInRange(t *testing.T) {
	m := newSelectMenu("t", 0, numberedItems(3), 2, nil)
	m.removeItem("id3")
	if m.selected != 1 || len(m.filtered) != 2 {
		t.Errorf("selected=%d filtered=%d, want 1 and 2", m.selected, len(m.filtered))
	}
	m.removeItem("id1")
	m.removeItem("id2")
	if m.selected != 0 || len(m.items) != 0 || len(m.filtered) != 0 {
		t.Errorf("empty menu: selected=%d items=%d filtered=%d", m.selected, len(m.items), len(m.filtered))
	}
	m.removeItem("missing") // unknown ID is harmless
}

func TestSelectMenuRenderRows(t *testing.T) {
	env := newMenuEnv(t)
	items := numberedItems(3)
	items[1].IsActive = true
	m := newTestMenu(env, items, 0, nil)
	m.render(true)
	lines := strings.Split(env.output(), "\n")
	// title, search, 7 item rows, footer, then the trailing empty split element
	if len(lines) != selectMenuLines+1 {
		t.Fatalf("rendered %d lines, want %d: %q", len(lines)-1, selectMenuLines, lines)
	}
	if strings.HasPrefix(lines[0], "\033[10A") {
		t.Errorf("first frame must not move the cursor up: %q", lines[0])
	}
	checks := []struct{ line, want string }{
		{lines[0], "pick one"},
		{lines[1], "> "},
		{lines[2], "❯"},
		{lines[2], "1. item 1"},
		{lines[3], "* "},
		{lines[3], " 2. item 2"},
		{lines[4], "   3. item 3"},
		{lines[8], "\033[2K"},
		{lines[9], "1/3 matches"},
	}
	for _, c := range checks {
		if !strings.Contains(c.line, c.want) {
			t.Errorf("line %q missing %q", c.line, c.want)
		}
	}
	if strings.Contains(lines[4], "❯") {
		t.Errorf("only the highlighted row has the pointer: %q", lines[4])
	}

	m.render(false)
	if out := env.output(); !strings.HasPrefix(out, "\033[10A") {
		t.Errorf("redraw should move up %d lines, got %q", selectMenuLines, out[:min(len(out), 12)])
	}
}

func TestSelectMenuRenderAccessibleGlyphs(t *testing.T) {
	env := newMenuEnv(t)
	theme.SetAccessibility(true)
	t.Cleanup(func() { theme.SetAccessibility(false) })
	m := newTestMenu(env, numberedItems(10), 8, nil)
	m.render(true)
	out := env.output()
	if !strings.Contains(out, "> ") || strings.Contains(out, "❯") {
		t.Errorf("accessible pointer should be '>': %q", out)
	}
	if !strings.Contains(out, "9/10 matches [^ 2 more, v 1 more]") {
		t.Errorf("accessible footer glyphs wrong: %q", out)
	}
	if strings.ContainsAny(out, "↑↓") {
		t.Errorf("accessible footer must use ASCII: %q", out)
	}
}

func TestSelectMenuClear(t *testing.T) {
	env := newMenuEnv(t)
	newTestMenu(env, numberedItems(1), 0, nil).clear()
	if got := env.output(); got != "\033[10A\033[0J" {
		t.Errorf("clear wrote %q", got)
	}
}

func TestClipEndAndStart(t *testing.T) {
	for _, tc := range []struct {
		s          string
		width      int
		end, start string
	}{
		{"hello", 5, "hello", "hello"},
		{"hello", 4, "hel…", "…llo"},
		{"hello", 1, "…", "…"},
		{"hello", 0, "", ""},
		{"日本語", 4, "日…", "…語"},
		{"↑/↓ move", 5, "↑/↓ …", "…move"},
	} {
		if got := clipEnd(tc.s, tc.width); got != tc.end {
			t.Errorf("clipEnd(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.end)
		}
		if got := clipStart(tc.s, tc.width); got != tc.start {
			t.Errorf("clipStart(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.start)
		}
		if w := VisibleWidth(clipEnd(tc.s, tc.width)); w > max(tc.width, 0) {
			t.Errorf("clipEnd(%q, %d) is %d columns wide", tc.s, tc.width, w)
		}
	}
}

// A row wider than the terminal wraps onto a second screen line, so the redraw, which moves
// up a fixed selectMenuLines rows, would leave the top row behind on every keypress.
func TestSelectMenuRowsFitTerminalWidth(t *testing.T) {
	env := newMenuEnv(t)
	items := numberedItems(2)
	items[0].Text = strings.Repeat("long conversation name ", 10)
	title := "select conversation (type to filter, ↑/↓ move, Enter select, Ctrl+E rename, Ctrl+D delete, Esc cancel)"
	m := newSelectMenu(title, int(os.Stdin.Fd()), items, 0, nil)
	m.query = strings.Repeat("q", 100)
	m.render(true)
	m.flashMsg = strings.Repeat("flash ", 30)
	m.render(false)
	width := getTerminalWidth()
	for line := range strings.SplitSeq(env.output(), "\n") {
		if w := VisibleWidth(line); w >= width {
			t.Errorf("row is %d columns, terminal has %d: %q", w, width, theme.StripANSI(line))
		}
	}
}
