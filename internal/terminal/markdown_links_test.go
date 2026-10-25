package terminal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/terminal/theme"
)

// TestMarkdownLinkParsing checks which text is taken for a [text](destination) link: nested
// brackets and parentheses are kept, and a destination with a space or no destination is not one.
func TestMarkdownLinkParsing(t *testing.T) {
	cases := []struct{ in, text, dest string }{
		{"[docs](https://x.dev/a) after", "docs", "https://x.dev/a"},
		{"[a [b] c](https://x.dev/f_(g)).", "a [b] c", "https://x.dev/f_(g)"},
		{"[`Foo.kt`](src/Foo.kt#L10)", "`Foo.kt`", "src/Foo.kt#L10"},
	}
	for _, c := range cases {
		text, dest, n, ok := markdownLink(c.in)
		if !ok || text != c.text || dest != c.dest || !strings.HasPrefix(c.in[n:], strings.TrimPrefix(c.in, "["+c.text+"]("+c.dest+")")) {
			t.Errorf("markdownLink(%q) = %q, %q, %d, %v", c.in, text, dest, n, ok)
		}
	}
	for _, in := range []string{"[a] (b)", "[a](b c)", "[a]()", "[](b)", "[a](b", "plain"} {
		if _, _, _, ok := markdownLink(in); ok {
			t.Errorf("markdownLink(%q) took it for a link", in)
		}
	}
}

// TestLinkTargets checks where a link points: a web page, or a file shown by its path relative
// to the working directory when it is inside it; anything else is not rendered.
func TestLinkTargets(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if web, file, _ := linkTarget("HTTPS://example.com/x"); web != "HTTPS://example.com/x" || file != "" {
		t.Errorf("web link: %q, %q", web, file)
	}
	cases := []struct{ dest, path, file string }{
		{"file://" + filepath.Join(wd, "shots", "a b.png"), "shots/a b.png", "file://" + filepath.Join(wd, "shots", "a%20b.png")},
		{"shots/a%20b.png#L3", "shots/a b.png#L3", "file://" + filepath.Join(wd, "shots", "a%20b.png")},
		{"/etc/hosts", "/etc/hosts", "file:///etc/hosts"},
		{"file://localhost/etc/hosts", "/etc/hosts", "file:///etc/hosts"},
	}
	for _, c := range cases {
		web, file, path := linkTarget(c.dest)
		if web != "" || file != c.file || path != c.path {
			t.Errorf("linkTarget(%q) = %q, %q, %q; want path %q, file %q", c.dest, web, file, path, c.path, c.file)
		}
	}
	for _, dest := range []string{"mailto:a@b.c", "#usage", "localhost:8080/x", "vscode://file/x", "file://otherhost/x"} {
		if web, file, _ := linkTarget(dest); web != "" || file != "" {
			t.Errorf("linkTarget(%q) = %q, %q; want nothing", dest, web, file)
		}
	}
}

// TestLinksRender checks a line's links: a web page's text is underlined and opens the page; a
// file shows its text and then its path, which opens the file; a link whose text is its path
// shows the path once; another link, and every link with colour off, is shown as written.
func TestLinksRender(t *testing.T) {
	useMarkdownTheme(t, false)
	md := theme.Markdown
	link := func(url, s string) string { return "\x1b]8;;" + url + "\x1b\\" + s + "\x1b]8;;\x1b\\" }
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	fileURL := "file://" + filepath.Join(wd, "shots", "a.png")
	cases := []struct{ in, want string }{
		{"see [the **docs**](https://x.dev) now", "see " + link("https://x.dev", md("the ", theme.MarkdownLink)+md("docs", theme.MarkdownLink|theme.MarkdownBold)) + " now"},
		{"[a.png](shots/a.png)", "a.png (" + link(fileURL, md("shots/a.png", theme.MarkdownCode)) + ")"},
		{"**[a.png](" + fileURL + ")**", md("a.png", theme.MarkdownBold) + md(" (", theme.MarkdownBold) + link(fileURL, md("shots/a.png", theme.MarkdownBold|theme.MarkdownCode)) + md(")", theme.MarkdownBold)},
		{"[shots/a.png](shots/a.png)", "shots/a.png"},
		{"[`shots/a.png`](shots/a.png)", md("shots/a.png", theme.MarkdownCode)},
		{"[mail](mailto:a@b.c) [x] (y)", "[mail](mailto:a@b.c) [x] (y)"},
	}
	for _, c := range cases {
		if got := RenderMarkdown(c.in); got != c.want {
			t.Errorf("RenderMarkdown(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	theme.SetEnabled(false)
	if in := "[a.png](shots/a.png) and [docs](https://x.dev)"; RenderMarkdown(in) != in {
		t.Errorf("with colour off: %q", RenderMarkdown(in))
	}
}

// TestWrappingKeepsLinkedPathsWhole checks that wrapping never cuts a linked word, such as a
// file's path longer than the line, which a cut would leave neither selectable nor openable
// whole, while other long words are still cut; and that a link wrapped across lines is opened
// again on each line and closed at its end, so a line never leaves a link open.
func TestWrappingKeepsLinkedPathsWhole(t *testing.T) {
	useMarkdownTheme(t, false)
	usePadding(t)
	long := strings.Repeat("d/", rowWidth()) + "shot.png"
	got := RenderMarkdown("- Shot: [shot.png](" + long + ")")
	var whole bool
	for l := range strings.SplitSeq(got, "\n") {
		if strings.Contains(theme.StripANSI(l), "("+long+")") {
			whole = true
		}
		closes := strings.Count(l, closeLink)
		if opens := strings.Count(l, "\x1b]8;;") - closes; opens != closes {
			t.Errorf("line %q leaves a link open", l)
		}
	}
	if !whole {
		t.Errorf("the path was cut:\n%s", theme.StripANSI(got))
	}

	words := strings.TrimSpace(strings.Repeat("word ", rowWidth()/5+5))
	got = RenderMarkdown("[" + words + "](https://x.dev)")
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("the link's text did not wrap: %q", got)
	}
	for _, l := range lines {
		if !strings.Contains(l, "\x1b]8;;https://x.dev\x1b\\") || !strings.HasSuffix(l, "\x1b]8;;\x1b\\") {
			t.Errorf("wrapped line %q does not open the link again and close it", l)
		}
	}
	if got := wrapStyled(strings.Repeat("x", 30), 10, 10); len(got) != 3 {
		t.Errorf("a word not in a link is no longer cut: %q", got)
	}
}

// TestTableCellLinks checks links in a table: a web page's label is underlined and opens it,
// with the column measured on the label alone; a file's label is followed by its path.
func TestTableCellLinks(t *testing.T) {
	useMarkdownTheme(t, false)
	got := RenderMarkdown("| Doc | Shot |\n|---|---|\n| [guide](https://x.dev/guide) | [a.png](shots/a.png) |\n")
	if !strings.Contains(got, "\x1b]8;;https://x.dev/guide\x1b\\") || !strings.Contains(got, "guide") {
		t.Errorf("no link to the guide in:\n%q", got)
	}
	plain := theme.StripANSI(got)
	if strings.Contains(plain, "https://") || !strings.Contains(plain, "a.png (shots/a.png)") {
		t.Errorf("table shows:\n%s", plain)
	}
	if lines := strings.Split(plain, "\n"); VisibleWidth(lines[0]) != VisibleWidth(lines[3]) {
		t.Errorf("columns are measured on more than the labels:\n%s", plain)
	}
}
