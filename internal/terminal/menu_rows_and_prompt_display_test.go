package terminal

import (
	"bufio"
	"strings"
	"testing"
)

// TestMenuLineKeepsItemsOnOneRow checks how item text from outside the program is shown in a
// menu row: escape sequences and control characters are removed, and Tabs and newlines
// become spaces so the row cannot wrap or break.
func TestMenuLineKeepsItemsOnOneRow(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain name", "plain name"},
		{"tab\tsep", "tab sep"},
		{"two\nlines", "two lines"},
		{"crlf\r\nline", "crlf line"},
		{"clear\x1b[2J\x1b[Hscreen", "clearscreen"},
		{"title\x1b]0;evil\x07set", "titleset"},
		{"bell\x07 back\x08space", "bell backspace"},
		{"c1 \u009b2J", "c1 2J"},
		{"日本語 🚀", "日本語 🚀"},
	}
	for _, tc := range tests {
		if got := menuLine(tc.in); got != tc.want {
			t.Errorf("menuLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPromptDisplayLineDrawsControlsAsOneColumn checks that a Tab in the prompt is drawn as
// the one column the layout gives it.
func TestPromptDisplayLineDrawsControlsAsOneColumn(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a\tb", "a b"},
		{"\t\tx", "  x"},
		{"no tabs", "no tabs"},
		{"日\t本", "日 本"},
	}
	for _, tc := range tests {
		if got := promptDisplayLine(tc.in); got != tc.want {
			t.Errorf("promptDisplayLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	ed := &PromptEditor{prompt: "> ", indent: "  ", runes: []rune("a\tb"), cursor: 3}
	if vl := ed.VisualLayout(80); vl.CursorVisualCol != 5 {
		t.Errorf("cursor column after \"a<Tab>b\" = %d, want 5 (2 for the prompt, 1 per character)", vl.CursorVisualCol)
	}
	ed = &PromptEditor{prompt: "> ", indent: "  ", runes: []rune("café"), cursor: 5}
	if vl := ed.VisualLayout(80); vl.CursorVisualCol != 6 {
		t.Errorf("cursor column after a combining accent = %d, want 6: the accent takes no column", vl.CursorVisualCol)
	}
}

// TestSelectWithoutTerminalNeedsAWholeNumber checks that an answer that only starts with a
// number, such as a provider named "2fast", is matched as text rather than as an item number.
func TestSelectWithoutTerminalNeedsAWholeNumber(t *testing.T) {
	items := []SelectItem{{ID: "x/one", Text: "x/one"}, {ID: "acme/a1", Text: "acme/a1"}, {ID: "2fast/gpt", Text: "2fast/gpt"}}
	tests := []struct{ answer, want string }{
		{"2fast/gpt\n", "2fast/gpt"},
		{"2\n", "acme/a1"},
		{" 3 \n", "2fast/gpt"},
		{"acme\n", "acme/a1"},
	}
	for _, tc := range tests {
		item, ok, err := selectWithoutTerminal(items, bufio.NewReader(strings.NewReader(tc.answer)))
		if err != nil || !ok || item.ID != tc.want {
			t.Errorf("answer %q chose (%q, %v, %v), want %q", tc.answer, item.ID, ok, err, tc.want)
		}
	}
	if _, ok, _ := selectWithoutTerminal(items, bufio.NewReader(strings.NewReader("4\n"))); ok {
		t.Error("an out-of-range number chose an item")
	}
}

// TestTableCellTabsBecomeSpaces checks that a Tab inside a cell is drawn as a space, so the
// cell keeps the width it was measured at.
func TestTableCellTabsBecomeSpaces(t *testing.T) {
	withTheme(t, false, false)
	tbl, ok := ParseMarkdownTable([]string{"| a\tb | c |", "|---|---|", "| x\ty | z |"})
	if !ok {
		t.Fatal("table not parsed")
	}
	out := tbl.Render()
	if strings.Contains(out, "\t") || !strings.Contains(out, "| a b |") || !strings.Contains(out, "| x y |") {
		t.Errorf("tabs not drawn as spaces:\n%s", out)
	}
}
