package terminal

import (
	"reflect"
	"strings"
	"testing"

	"hmj-agent/internal/terminal/theme"
)

// withPlainTheme turns colour and accessibility off for one test and restores them after.
func withTheme(t *testing.T, colour, accessible bool) {
	t.Helper()
	theme.SetEnabled(colour)
	theme.SetAccessibility(accessible)
	t.Cleanup(func() {
		theme.SetEnabled(false)
		theme.SetAccessibility(false)
	})
}

func TestRuneAndVisibleWidth(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want int
	}{
		{"ascii", "hello", 5},
		{"control rune is zero", "\x01a", 1},
		{"CJK is double width", "日本語", 6},
		{"hangul syllables are double width", "한국", 4},
		{"fullwidth form is double width", "Ａ", 2},
		{"emoji block is double width", "🙂", 2},
		{"misc symbol narrow", "\u2603", 1}, // snowman: not in the wide list
		{"misc symbol wide", "\u2615", 2},   // hot beverage: wide
		{"ANSI does not count", "\x1b[31mred\x1b[0m", 3},
		{"combining mark takes no column", "e\u0301", 1},
		{"enclosing mark takes no column", "1\u20e3", 1},
		{"zero-width joiner takes no column", "a\u200db", 2},
		{"variation selector takes no column", "\u2764\ufe0f", 1},
		{"hangul jamo vowel takes no column", "\u1100\u1161", 2},
		{"soft hyphen keeps its column", "a\u00adb", 3},
		{"transport emoji is double width", "🚀", 2},
		{"extended-A emoji is double width", "\U0001fae0", 2},
		{"coloured circle is double width", "\U0001f7e0", 2},
		{"watch and hourglass are double width", "\u231a\u23f3", 4},
		{"star is double width", "\u2b50", 2},
		{"mahjong tile is double width", "\U0001f004", 2},
		{"enclosed ideograph is double width", "\U0001f201", 2},
		{"CJK extension B is double width", "\U00020000", 2},
		{"empty", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VisibleWidth(tc.s); got != tc.want {
				t.Errorf("VisibleWidth(%q) = %d, want %d", tc.s, got, tc.want)
			}
		})
	}
	if runeWidth('\n') != 0 || runeWidth('A') != 1 || runeWidth('日') != 2 {
		t.Error("runeWidth basic cases wrong")
	}
}

func TestClipEndAndStartWideRunes(t *testing.T) {
	if got := clipEnd("日本語テキスト", 5); got != "日本…" {
		t.Errorf("clipEnd wide = %q, want 日本…", got)
	}
	if got := clipStart("日本語テキスト", 5); got != "…スト" {
		t.Errorf("clipStart wide = %q, want …スト (width 5)", got)
	}
	if got := clipEnd("abc", 0); got != "" {
		t.Errorf("clipEnd zero width = %q", got)
	}
	if got := clipStart("abc", 0); got != "" {
		t.Errorf("clipStart zero width = %q", got)
	}
	if got := clipEnd("abc", 3); got != "abc" {
		t.Errorf("clipEnd fits = %q", got)
	}
	if got := clipStart("abc", 3); got != "abc" {
		t.Errorf("clipStart fits = %q", got)
	}
}

func TestPadCellAlignments(t *testing.T) {
	tests := []struct {
		s     string
		width int
		align Alignment
		want  string
	}{
		{"ab", 5, AlignLeft, "ab   "},
		{"ab", 5, AlignRight, "   ab"},
		{"ab", 5, AlignCenter, " ab  "},
		{"日本", 6, AlignRight, "  日本"},
	}
	for _, tc := range tests {
		if got := padCell(tc.s, tc.width, tc.align); got != tc.want {
			t.Errorf("padCell(%q, %d, %v) = %q, want %q", tc.s, tc.width, tc.align, got, tc.want)
		}
	}
}

func TestRenderCellMarkdown(t *testing.T) {
	withTheme(t, true, false)
	// With colour on, styled spans must still measure as their plain text: exactly what
	// stripCellMarkdown leaves, which is what the column widths are computed from. Unmatched
	// markers stay literal, a code span keeps its asterisks, and markers nested in bold are
	// dropped.
	tests := []struct {
		in        string
		wantPlain string
	}{
		{"plain", "plain"},
		{"**bold** word", "bold word"},
		{"use `code` here", "use code here"},
		{"*it* ok", "it ok"},
		{"unclosed `code", "unclosed `code"},
		{"unclosed *ital", "unclosed *ital"},
		{"5 * 3", "5 * 3"},
		{"5 * 3 * 2", "5 * 3 * 2"},
		{"**half of a bold span", "**half of a bold span"},
		{"**a *nested* b**", "a nested b"},
		{"`*.go` files", "*.go files"},
		{"`a ** b` and *x*", "a ** b and x"},
		{"empty `` ticks", "empty `` ticks"},
		{"a * b *c*", "a * b c"},
	}
	for _, tc := range tests {
		got := parseCellMarkdown(tc.in).render(true)
		if plain := theme.StripANSI(got); plain != tc.wantPlain || plain != stripCellMarkdown(tc.in) {
			t.Errorf("render of %q: plain = %q, want %q (stripCellMarkdown %q)", tc.in, plain, tc.wantPlain, stripCellMarkdown(tc.in))
		}
	}
	if got := parseCellMarkdown("**bold** and `x`").render(true); got == "**bold** and `x`" {
		t.Error("markdown markers were not consumed when colour is on")
	}
}

func TestRenderCellMarkdownStripsMarkersWithoutUnicode(t *testing.T) {
	withTheme(t, false, false)
	if got := parseCellMarkdown("**bold** `code`").render(false); got != stripCellMarkdown("**bold** `code`") {
		t.Errorf("non-unicode cell = %q, want stripCellMarkdown output", got)
	}
}

func TestWrapCellBreaksAndKeepsBreaks(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{"zero width returns text", "abc def", 0, []string{"abc def"}},
		{"explicit <br/>", "one<br/>two", 10, []string{"one", "two"}},
		{"<br> with newline", "a<br>b\nc", 10, []string{"a", "b", "c"}},
		{"empty cell", "", 5, []string{""}},
		{"wraps on space", "aa bb cc", 5, []string{"aa bb", "cc"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := plainLines(wrapCell(tc.in, tc.width))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("wrapCell(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
			}
		})
	}
}

func TestWrapCellCJKRespectsWidth(t *testing.T) {
	for _, line := range plainLines(wrapCell("日本語のテキストを折り返す", 6)) {
		if w := VisibleWidth(line); w > 6 {
			t.Errorf("wrapped line %q is %d columns, want <= 6", line, w)
		}
	}
}

// TestWrapCellKeepsStyleAcrossLines checks that a styled span cut by a line break keeps its
// style on both lines instead of showing its markers, and that a literal asterisk survives.
func TestWrapCellKeepsStyleAcrossLines(t *testing.T) {
	got := wrapCell("**bold words here** and `x*y`", 10)
	want := []cellLine{
		{{"bold", styleBold, ""}, {" ", stylePlain, ""}, {"words", styleBold, ""}},
		{{"here", styleBold, ""}, {" ", stylePlain, ""}, {"and", stylePlain, ""}},
		{{"x*y", styleCode, ""}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wrapCell = %#v, want %#v", got, want)
	}
}

// TestSplitCellWordKeepsStyles checks that a word wider than its column is cut by display
// width across spans of different styles.
func TestSplitCellWordKeepsStyles(t *testing.T) {
	got, lastW := splitCellWord(cellLine{{"abc", styleBold, ""}, {"de", stylePlain, ""}}, 2)
	want := []cellLine{{{"ab", styleBold, ""}}, {{"c", styleBold, ""}, {"d", stylePlain, ""}}, {{"e", stylePlain, ""}}}
	if !reflect.DeepEqual(got, want) || lastW != 1 {
		t.Errorf("splitCellWord = %#v, %d; want %#v, 1", got, lastW, want)
	}
}

// plainLines returns the visible text of each line.
func plainLines(lines []cellLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.plain()
	}
	return out
}

func TestComputeColumnWidthsFitsBudget(t *testing.T) {
	headers := []string{"name", "description"}
	rows := [][]string{
		{"short", strings.Repeat("word ", 40)},
		{"a much longer name for the first column", "x"},
	}
	for _, budget := range []int{40, 80} {
		widths := computeColumnWidths(headers, rows, budget)
		if len(widths) != 2 {
			t.Fatalf("budget %d: got %d widths", budget, len(widths))
		}
		for i, w := range widths {
			if w < 1 {
				t.Errorf("budget %d: column %d width %d < 1", budget, i, w)
			}
		}
		if sum := widths[0] + widths[1]; sum > budget && sum > len(headers) {
			t.Errorf("budget %d: widths %v sum to %d", budget, widths, sum)
		}
	}
}

func TestComputeColumnWidthsNaturalWhenItFits(t *testing.T) {
	headers := []string{"a", "bb"}
	rows := [][]string{{"xxx", "y"}}
	got := computeColumnWidths(headers, rows, 200)
	if got[0] != 3 || got[1] != 2 {
		t.Errorf("widths = %v, want natural [3 2]", got)
	}
}

func TestRenderTableWidthBounds(t *testing.T) {
	// getTerminalWidth falls back to 80 when stdout and stdin are not terminals (tests).
	if got := renderTableWidth(); got < 40 || got > 140 {
		t.Errorf("renderTableWidth = %d, want within [40,140]", got)
	}
}

func TestTableRenderASCIIBorders(t *testing.T) {
	withTheme(t, false, false)
	tbl := &Table{
		Headers: []string{"Name", "Count"},
		Align:   []Alignment{AlignLeft, AlignRight},
		Rows:    [][]string{{"alpha", "3"}, {"日本", "12"}},
	}
	out := tbl.Render()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 6 {
		t.Fatalf("rendered %d lines, want at least 6:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "+") || !strings.HasPrefix(lines[len(lines)-1], "+") {
		t.Errorf("ASCII borders missing:\n%s", out)
	}
	if strings.ContainsAny(out, "┌┐└┘│─") {
		t.Errorf("box-drawing glyphs used in ASCII mode:\n%s", out)
	}
	width := VisibleWidth(lines[0])
	for _, l := range lines {
		if VisibleWidth(l) != width {
			t.Errorf("line %q is %d wide, want %d:\n%s", l, VisibleWidth(l), width, out)
		}
	}
}

func TestTableRenderUnicodeBordersWithColour(t *testing.T) {
	withTheme(t, true, false)
	tbl := &Table{Headers: []string{"**Key**"}, Align: []Alignment{AlignCenter}, Rows: [][]string{{"`v`"}}}
	out := tbl.Render()
	if !strings.Contains(theme.StripANSI(out), "┌") {
		t.Errorf("unicode border missing:\n%s", out)
	}
}

func TestTableRenderEmptyHeadersIsEmpty(t *testing.T) {
	if got := (&Table{}).Render(); got != "" {
		t.Errorf("Render with no headers = %q, want empty", got)
	}
}

func TestTableRenderWrapsLongCells(t *testing.T) {
	withTheme(t, false, false)
	long := strings.Repeat("lorem ipsum dolor sit amet ", 12)
	tbl := &Table{Headers: []string{"text"}, Align: []Alignment{AlignLeft}, Rows: [][]string{{long}}}
	out := tbl.Render()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) <= 6 {
		t.Errorf("long cell was not wrapped onto several lines:\n%s", out)
	}
	limit := renderTableWidth()
	for _, l := range lines {
		if VisibleWidth(l) > limit {
			t.Errorf("line is %d columns, budget %d: %q", VisibleWidth(l), limit, l)
		}
	}
}

func TestParseMarkdownTableAlignmentAndRagged(t *testing.T) {
	lines := []string{
		"| a | b | c |",
		"|:--|--:|:-:|",
		"| 1 | 2 |",
		"| 3 | 4 | 5 | 6 |",
	}
	tbl, ok := ParseMarkdownTable(lines)
	if !ok {
		t.Fatal("ParseMarkdownTable rejected a valid table")
	}
	if len(tbl.Align) != 3 || tbl.Align[0] != AlignLeft || tbl.Align[1] != AlignRight || tbl.Align[2] != AlignCenter {
		t.Errorf("alignments = %v", tbl.Align)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("rows = %d", len(tbl.Rows))
	}
	if _, ok := ParseMarkdownTable([]string{"| only header |"}); ok {
		t.Error("table without separator row should be rejected")
	}
}

func TestMarkdownStreamerRendersTableAndPassesProse(t *testing.T) {
	withTheme(t, false, false)
	var out strings.Builder
	s := NewMarkdownStreamer(func(x string) { out.WriteString(x) })
	s.Write("intro line\n| h1 | h2 |\n|---|---|\n| a | b |\nafter\n")
	s.Flush()
	got := out.String()
	if !strings.HasPrefix(got, "intro line\n") {
		t.Errorf("prose before table not passed through first: %q", got)
	}
	if !strings.Contains(got, "+") || !strings.Contains(got, "h1") {
		t.Errorf("table not rendered:\n%s", got)
	}
	if !strings.HasSuffix(got, "after\n") {
		t.Errorf("prose after table lost: %q", got)
	}
}

func TestMarkdownStreamerChunkedAndUnparseableTable(t *testing.T) {
	var out strings.Builder
	s := NewMarkdownStreamer(func(x string) { out.WriteString(x) })
	// Chunk boundaries fall mid-line; the rows lack a separator so they are echoed raw.
	s.Write("| x | y |\n| p ")
	s.Write("| q |\n")
	s.Write("trailing without newline")
	s.Flush()
	got := out.String()
	if !strings.Contains(got, "| x | y |\n| p | q |\n") {
		t.Errorf("unparseable table lines not echoed raw:\n%q", got)
	}
	if !strings.HasSuffix(got, "trailing without newline") {
		t.Errorf("unterminated final line not flushed: %q", got)
	}
}

func TestMarkdownStreamerFlushWithPendingTableRow(t *testing.T) {
	// A table already in progress keeps a final row that has no newline.
	var out strings.Builder
	s := NewMarkdownStreamer(func(x string) { out.WriteString(x) })
	s.Write("| a | b |\n|---|---|\n| 1 | 2 |")
	s.Flush()
	if !strings.Contains(out.String(), "1") || !strings.Contains(out.String(), "2") {
		t.Errorf("pending table row lost on Flush: %q", out.String())
	}
}

func TestMarkdownStreamerFlushEmitsPendingProse(t *testing.T) {
	var out strings.Builder
	s := NewMarkdownStreamer(func(x string) { out.WriteString(x) })
	s.Write("| a | b |\n|---|---|\n| 1 | 2 |\nplain tail")
	s.Flush()
	got := out.String()
	if !strings.HasSuffix(got, "plain tail") || !strings.Contains(got, "1") {
		t.Errorf("Flush output = %q", got)
	}
}
