package terminal

import (
	"strings"
	"testing"

	"hmj-agent/v2/internal/terminal/theme"
)

// useMarkdownTheme sets colour on and accessibility mode as given for the rest of the test.
func useMarkdownTheme(t *testing.T, accessible bool) {
	t.Helper()
	enabled := theme.Enabled()
	theme.SetEnabled(true)
	theme.SetAccessibility(accessible)
	t.Cleanup(func() {
		theme.SetEnabled(enabled)
		theme.SetAccessibility(false)
	})
}

// TestRenderMarkdownLines checks each kind of line the transcript renders, nested styles included.
func TestRenderMarkdownLines(t *testing.T) {
	useMarkdownTheme(t, false)
	md := theme.Markdown
	cases := []struct{ in, want string }{
		{"**Findings**", md("Findings", theme.MarkdownBold)},
		{"say **bold `x` here** now", "say " + md("bold ", theme.MarkdownBold) + md("x", theme.MarkdownBold|theme.MarkdownCode) + md(" here", theme.MarkdownBold) + " now"},
		{"*it* and **b**:", md("it", theme.MarkdownItalic) + " and " + md("b", theme.MarkdownBold) + ":"},
		{"*a **b** c*", md("a ", theme.MarkdownItalic) + md("b", theme.MarkdownItalic|theme.MarkdownBold) + md(" c", theme.MarkdownItalic)},
		{"run `go test ./...` now", "run " + md("go test ./...", theme.MarkdownCode) + " now"},
		{"``a ` b``", md("a ` b", theme.MarkdownCode)},
		{"## Title **x** ##", md("Title ", theme.MarkdownHeading) + md("x", theme.MarkdownHeading|theme.MarkdownBold)},
		{"- item `a`", "• item " + md("a", theme.MarkdownCode)},
		{"  * nested", "  • nested"},
		{"3. step **one**", "3. step " + md("one", theme.MarkdownBold)},
		{"> quoted *text*", theme.Faint("│") + " quoted " + md("text", theme.MarkdownItalic)},
		{"---", theme.Rule(min(rowWidth(), 80))},
		{"* * *", theme.Rule(min(rowWidth(), 80))},
		{`\*not italic\*`, "*not italic*"},
	}
	for _, tc := range cases {
		var b markdownBlock
		if got := b.renderLine(tc.in).text; got != tc.want {
			t.Errorf("renderLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRenderMarkdownKeepsWhatIsNotMarkup checks that text whose markers have no pair on the line,
// or are not markup, is shown exactly as written.
func TestRenderMarkdownKeepsWhatIsNotMarkup(t *testing.T) {
	useMarkdownTheme(t, false)
	for _, in := range []string{
		"2 * 3 = 6", "2*3*4", "snake_case_name and __init__", "a ** b", "**open but never closed",
		"x**2 + y**2", "`unclosed code", `C:\path\to and regex \d+\.`, "#hashtag", "plain text",
		"** spaced **", "*", "1.5 is a number", "####", "# ",
	} {
		var b markdownBlock
		if got := b.renderLine(in).text; got != in {
			t.Errorf("renderLine(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestRenderMarkdownRendersRowsThatAreNotATable checks that lines held as a possible table but
// not parsed as one are rendered like any other line.
func TestRenderMarkdownRendersRowsThatAreNotATable(t *testing.T) {
	useMarkdownTheme(t, false)
	got := RenderMarkdown("|x| and **b**|\nnext")
	want := "|x| and " + theme.Markdown("b", theme.MarkdownBold) + "|\nnext"
	if got != want {
		t.Errorf("RenderMarkdown = %q, want %q", got, want)
	}
}

// TestRenderMarkdownLeavesCodeFencesAlone checks that lines in a code fence are shown as written,
// table rows included, and only the fence lines are dimmed.
func TestRenderMarkdownLeavesCodeFencesAlone(t *testing.T) {
	useMarkdownTheme(t, false)
	in := "```go\n**x** := 1\n| a | b |\n|---|---|\n````\n```\n~~~\n# not a heading\n~~~\n**after**\n"
	want := theme.Faint("```go") + "\n**x** := 1\n| a | b |\n|---|---|\n" + theme.Faint("````") + "\n" +
		theme.Faint("```") + "\n" + // a fence of 4 closes one of 3; this one opens a new block
		"~~~\n# not a heading\n~~~\n" + "**after**"
	got := RenderMarkdown(strings.TrimSuffix(in, "\n"))
	if got != want {
		t.Errorf("RenderMarkdown =\n%q\nwant\n%q", got, want)
	}

	got = RenderMarkdown("```\nx\n```\n**after**")
	if want := theme.Faint("```") + "\nx\n" + theme.Faint("```") + "\n" + theme.Markdown("after", theme.MarkdownBold); got != want {
		t.Errorf("after a closed fence = %q, want %q", got, want)
	}
	if !strings.Contains(got, "\x1b[1m") {
		t.Errorf("no bold sequence in %q", got)
	}
}

// TestRenderMarkdownWithoutColorIsRaw checks that with colour off the text is shown exactly as
// written, so its markers still carry the emphasis.
func TestRenderMarkdownWithoutColorIsRaw(t *testing.T) {
	useMarkdownTheme(t, false)
	theme.SetEnabled(false)
	in := "# Title\n**bold** and *it* and `code`\n- item\n> quote\n---\n```\ncode\n```\n"
	if got := RenderMarkdown(in); got != in {
		t.Errorf("RenderMarkdown without colour =\n%q\nwant\n%q", got, in)
	}
}

// TestMarkdownStreamerRendersChunksLikeWholeText checks that text arriving a character at a time
// renders exactly as the same text in one piece.
func TestMarkdownStreamerRendersChunksLikeWholeText(t *testing.T) {
	useMarkdownTheme(t, false)
	in := "## Plan\n- **first** step with `code`\n```\n**raw**\n```\n| a | b |\n|---|---|\n| **x** | y |\nend *here*"
	var got strings.Builder
	s := NewMarkdownStreamer(func(out string) { got.WriteString(out) })
	for _, r := range in {
		s.Write(string(r))
	}
	s.Flush()
	if want := RenderMarkdown(in); got.String() != want {
		t.Errorf("streamed =\n%q\nwhole =\n%q", got.String(), want)
	}
}

// TestRenderMarkdownAccessible checks that accessibility mode draws bullets, quotes and rules in
// ASCII.
func TestRenderMarkdownAccessible(t *testing.T) {
	useMarkdownTheme(t, true)
	var b markdownBlock
	if got := b.renderLine("- item").text; got != "- item" {
		t.Errorf("bullet = %q, want %q", got, "- item")
	}
	if got := b.renderLine("> q").text; got != theme.Faint("|")+" q" {
		t.Errorf("quote = %q", got)
	}
	if got := theme.StripANSI(b.renderLine("***").text); strings.Trim(got, "-") != "" || got == "" {
		t.Errorf("rule = %q, want dashes", got)
	}
}

// usePadding sets the margin and wrapping on for the rest of the test.
func usePadding(t *testing.T) {
	t.Helper()
	saved := padMarkdown
	padMarkdown = func() bool { return true }
	t.Cleanup(func() { padMarkdown = saved })
}

// TestPaddedMarkdownWrapsWithAMargin checks that on a terminal every line of the model's text
// starts after the margin and fits the window, wrapped lines of a list item hang under its text,
// a style broken across lines is set again on the next, and no word is lost or split.
func TestPaddedMarkdownWrapsWithAMargin(t *testing.T) {
	useMarkdownTheme(t, false)
	usePadding(t)
	width := rowWidth()
	words := strings.Repeat("lorem ipsum dolor sit amet ", 12)
	in := "plain " + words + "\n- item **" + words + "end** done\n12. step " + words
	got := RenderMarkdown(in)
	lines := strings.Split(got, "\n")
	if len(lines) < 9 {
		t.Fatalf("not wrapped: %q", got)
	}
	for i, l := range lines {
		plain := theme.StripANSI(l)
		if w := VisibleWidth(plain); w > width {
			t.Errorf("line %d is %d columns, over %d: %q", i, w, width, plain)
		}
		if !strings.HasPrefix(plain, "  ") {
			t.Errorf("line %d lacks the margin: %q", i, plain)
		}
	}
	var item, step []string
	for _, l := range lines {
		plain := theme.StripANSI(l)
		switch {
		case strings.HasPrefix(plain, "  • "), len(item) > 0 && len(step) == 0 && strings.HasPrefix(plain, "    ") && !strings.HasPrefix(plain, "     "):
			item = append(item, l)
		case strings.HasPrefix(plain, "  12. "), len(step) > 0 && strings.HasPrefix(plain, "      "):
			step = append(step, l)
		}
	}
	if len(item) < 2 || len(step) < 2 {
		t.Fatalf("list item or step did not wrap with a hang:\n%s", theme.StripANSI(got))
	}
	for _, l := range item[1:] {
		if !strings.HasPrefix(l, "    ") || !strings.Contains(l, "\x1b[1m") {
			t.Errorf("wrapped item line %q should hang under the text and stay bold", l)
		}
	}
	if joined := strings.Join(strings.Fields(theme.StripANSI(got)), " "); joined != strings.Join(strings.Fields(strings.NewReplacer("**", "", "- item", "• item").Replace(in)), " ") {
		t.Errorf("words changed by wrapping:\n%s", joined)
	}
}

// TestPaddedMarkdownLeavesCodeAndTablesUnwrapped checks that code in fences and tables get the
// margin but are not wrapped, and blank lines get no trailing spaces.
func TestPaddedMarkdownLeavesCodeAndTablesUnwrapped(t *testing.T) {
	useMarkdownTheme(t, false)
	usePadding(t)
	long := strings.Repeat("x ", rowWidth())
	got := RenderMarkdown("```\n" + long + "\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n")
	lines := strings.Split(got, "\n")
	if lines[1] != "  "+long {
		t.Errorf("code line = %q, want it after the margin, whole", lines[1])
	}
	if lines[3] != "" {
		t.Errorf("blank line = %q, want empty", lines[3])
	}
	for _, l := range lines[4:] {
		if l != "" && !strings.HasPrefix(theme.StripANSI(l), "  ") {
			t.Errorf("table line %q lacks the margin", theme.StripANSI(l))
		}
	}
}

// TestWrapStyledCutsWordsLongerThanALine checks that a word wider than a line is cut, not left
// to overflow, and that leading indentation is not taken for a place to break.
func TestWrapStyledCutsWordsLongerThanALine(t *testing.T) {
	if got := wrapStyled("abcdefghij", 4, 4); strings.Join(got, "|") != "abcd|efgh|ij" {
		t.Errorf("wrapStyled = %q", got)
	}
	if got := wrapStyled("    longword", 6, 6); strings.Join(got, "|") != "    lo|ngword" {
		t.Errorf("wrapStyled with indent = %q", got)
	}
	if got := wrapStyled("ab cd", 2, 2); strings.Join(got, "|") != "ab|cd" {
		t.Errorf("wrapStyled at a space = %q", got)
	}
}
