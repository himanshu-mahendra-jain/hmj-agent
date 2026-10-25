package terminal

import (
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"hmj-agent/internal/terminal/theme"
)

// markdownPad is the margin the model's text is shown with on a terminal.
const markdownPad = "  "

// padMarkdown reports whether the model's text is shown with markdownPad before it and wrapped to
// the window: on a terminal, outside accessibility mode, where a screen reader reads each line as
// it comes and breaks inside sentences would only get in the way.
var padMarkdown = func() bool { return !theme.Accessibility() && theme.IsTerminal(os.Stdout.Fd()) }

// The model's Markdown is rendered one line at a time, as each line is complete: headings, bullet
// and numbered lists, quotes, rules, code fences, and **bold**, *italic* and `code` within a line.
// A marker is taken out only when its pair closes on the same line, so nothing the model wrote is
// lost; anything else is shown as written. With colour off the text is shown as written, since
// the markers are then the only emphasis left. Code in fences is never styled.

var (
	headingLine  = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(\S.*?)(?:[ \t]+#+)?[ \t]*$`)
	ruleLine     = regexp.MustCompile(`^ {0,3}(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	bulletLine   = regexp.MustCompile(`^([ \t]*)[-*+][ \t]+(.*)$`)
	numberedLine = regexp.MustCompile(`^([ \t]*\d{1,9}[.)])[ \t]+(.*)$`)
	quoteLine    = regexp.MustCompile(`^ {0,3}>[ \t]?(.*)$`)
	fenceLine    = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
)

// markdownBlock is what carries over from one line of Markdown to the next: the code fence the
// text is in.
type markdownBlock struct {
	fence string // the fence that opened the current code block, such as "```", or ""
	pad   bool   // whether lines are shown with markdownPad before them
}

// inFence reports whether the next line is inside a code block.
func (b *markdownBlock) inFence() bool { return b.fence != "" }

// renderedLine is one line of Markdown as it is shown.
type renderedLine struct {
	text string
	// wrap says whether the line may be wrapped to the window, and hang is what its wrapped
	// lines start with: spaces as wide as a list marker, or a quote's bar.
	wrap bool
	hang string
}

// renderLine returns line as it is shown, without its line break.
func (b *markdownBlock) renderLine(line string) renderedLine {
	if b.fence != "" {
		if closesFence(line, b.fence) {
			b.fence = ""
			return renderedLine{text: theme.Faint(line)}
		}
		return renderedLine{text: line}
	}
	if m := fenceLine.FindStringSubmatch(line); m != nil && (m[1][0] == '~' || !strings.Contains(m[2], "`")) {
		b.fence = m[1]
		return renderedLine{text: theme.Faint(line)}
	}
	styled := theme.Enabled()
	if m := headingLine.FindStringSubmatch(line); m != nil && styled {
		var sb strings.Builder
		writeInline(&sb, m[1], theme.MarkdownHeading)
		return renderedLine{text: sb.String(), wrap: true}
	}
	if ruleLine.MatchString(line) && styled {
		return renderedLine{text: theme.Rule(min(b.width(), 80))}
	}
	if m := bulletLine.FindStringSubmatch(line); m != nil {
		r := renderedLine{text: line, wrap: true, hang: m[1] + "  "}
		if styled {
			bullet := "•"
			if theme.Accessibility() {
				bullet = "-"
			}
			r.text = m[1] + bullet + " " + renderInline(m[2])
		}
		return r
	}
	if m := numberedLine.FindStringSubmatch(line); m != nil {
		r := renderedLine{text: line, wrap: true, hang: strings.Repeat(" ", VisibleWidth(m[1])+1)}
		if styled {
			r.text = m[1] + " " + renderInline(m[2])
		}
		return r
	}
	if m := quoteLine.FindStringSubmatch(line); m != nil && styled {
		bar := theme.Faint(theme.ASCII("│")) + " "
		return renderedLine{text: bar + renderInline(m[1]), wrap: true, hang: bar}
	}
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	if !styled {
		return renderedLine{text: line, wrap: true, hang: indent}
	}
	return renderedLine{text: renderInline(line), wrap: true, hang: indent}
}

// width returns how many columns a line of the model's text may take.
func (b *markdownBlock) width() int {
	if b.pad {
		return max(rowWidth()-len(markdownPad), 10)
	}
	return rowWidth()
}

// show returns line as it is printed: rendered, and when padded, after the margin and wrapped
// to the window, its wrapped lines starting with the margin and the line's hang.
func (b *markdownBlock) show(line string) string {
	r := b.renderLine(line)
	if !b.pad || r.text == "" {
		return r.text
	}
	if !r.wrap {
		return markdownPad + r.text
	}
	width := b.width()
	lines := wrapStyled(r.text, width, max(width-VisibleWidth(r.hang), 10))
	return markdownPad + strings.Join(lines, "\n"+markdownPad+r.hang)
}

// padBlock puts the margin before each line of text that is not empty.
func padBlock(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = markdownPad + l
		}
	}
	return strings.Join(lines, "\n")
}

// closesFence reports whether line ends the code block that fence opened: a run of the same
// character, at least as long, and nothing else.
func closesFence(line, fence string) bool {
	trimmed := strings.TrimRight(line, " \t")
	body := strings.TrimLeft(trimmed, " ")
	if len(trimmed)-len(body) > 3 || len(body) < len(fence) {
		return false
	}
	return strings.Trim(body, fence[:1]) == ""
}

// renderInline styles **bold**, *italic* and `code` in s, and drops the backslash of an escaped
// marker.
func renderInline(s string) string {
	var sb strings.Builder
	writeInline(&sb, s, 0)
	return sb.String()
}

// writeInline writes s to sb in the style st, with the spans s marks out in their own styles
// added to it.
func writeInline(sb *strings.Builder, s string, st theme.MarkdownStyle) {
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			sb.WriteString(theme.Markdown(run.String(), st))
			run.Reset()
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isEscapable(s[i+1]):
			run.WriteByte(s[i+1])
			i += 2
			continue
		case c == '`':
			n := markerRun(s, i, '`')
			if end := closingTicks(s, i+n, n); end > i+n {
				flush()
				sb.WriteString(theme.Markdown(s[i+n:end], st|theme.MarkdownCode))
				i = end + n
				continue
			}
			run.WriteString(s[i : i+n])
			i += n
			continue
		case c == '[':
			if text, dest, n, ok := markdownLink(s[i:]); ok {
				if webURL, fileURL, path := linkTarget(dest); webURL != "" || fileURL != "" {
					flush()
					writeLink(sb, text, webURL, fileURL, path, st)
					i += n
					continue
				}
			}
		case c == '*':
			n := markerRun(s, i, '*')
			if n <= 2 {
				if end := closingEmphasis(s, i, n); end >= 0 {
					flush()
					style := theme.MarkdownItalic
					if n == 2 {
						style = theme.MarkdownBold
					}
					writeInline(sb, s[i+n:end], st|style)
					i = end + n
					continue
				}
			}
			run.WriteString(s[i : i+n])
			i += n
			continue
		}
		run.WriteByte(c)
		i++
	}
	flush()
}

// writeLink writes a link in the style st: a web page's as its text, underlined, which the
// terminal opens; a file's as its text and then its path, unless the text is the path.
func writeLink(sb *strings.Builder, text, webURL, fileURL, path string, st theme.MarkdownStyle) {
	var label strings.Builder
	if webURL != "" {
		writeInline(&label, text, st|theme.MarkdownLink)
		sb.WriteString(theme.Hyperlink(webURL, label.String()))
		return
	}
	writeInline(&label, text, st)
	sb.WriteString(label.String())
	if theme.StripANSI(label.String()) != path {
		sb.WriteString(theme.Markdown(" (", st))
		sb.WriteString(theme.Hyperlink(fileURL, theme.Markdown(path, st|theme.MarkdownCode)))
		sb.WriteString(theme.Markdown(")", st))
	}
}

// isEscapable reports whether a backslash before c is Markdown's escape, which is not shown. Only
// the markers rendered here are escapable, so a backslash in a path or a regex stays.
func isEscapable(c byte) bool {
	return c == '*' || c == '`' || c == '_' || c == '#' || c == '\\'
}

// markerRun returns how many times c repeats in s from i.
func markerRun(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// closingTicks returns where a run of exactly n backticks starts in s from i, or -1.
func closingTicks(s string, i, n int) int {
	for j := i; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		m := markerRun(s, j, '`')
		if m == n {
			return j
		}
		j += m
	}
	return -1
}

// closingEmphasis returns where the run of n asterisks that closes the one at open starts, or -1.
// The opening run must not follow a letter or digit or come before a space, and the closing one
// must not follow a space or come before a letter or digit, so 2*3*4 and a lone * stay as
// written. Code spans and escapes in between are skipped, as are runs of another length.
func closingEmphasis(s string, open, n int) int {
	start := open + n
	if start >= len(s) || isSpace(s[start]) || open > 0 && isAlnum(s[open-1]) {
		return -1
	}
	for j := start; j < len(s); {
		switch s[j] {
		case '\\':
			j += 2
			continue
		case '`':
			m := markerRun(s, j, '`')
			if end := closingTicks(s, j+m, m); end > j+m {
				j = end + m
			} else {
				j += m
			}
			continue
		case '*':
			m := markerRun(s, j, '*')
			if m == n && j > start && !isSpace(s[j-1]) && (j+m >= len(s) || !isAlnum(s[j+m])) {
				return j
			}
			j += m
			continue
		}
		j++
	}
	return -1
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' }

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// RenderMarkdown returns text rendered as the transcript shows the model's answers as they
// stream: Markdown styled and tables drawn.
func RenderMarkdown(text string) string {
	var sb strings.Builder
	s := NewMarkdownStreamer(func(out string) { sb.WriteString(out) })
	s.Write(text)
	s.Flush()
	return sb.String()
}

// styledRune is a visible rune of styled text, the escape sequences in effect for it and the
// URL of the link it is in, if any.
type styledRune struct {
	r     rune
	style string
	link  string
}

// parseStyled splits s into its visible runes, each with the SGR sequences set before it since
// the last reset and the URL of the OSC 8 link it is in.
func parseStyled(s string) []styledRune {
	out := make([]styledRune, 0, utf8.RuneCountInString(s)) // the escapes count too, but rarely
	style, link := "", ""
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == ']' {
			end := strings.Index(s[i:], "\x1b\\")
			if end < 0 {
				end = len(s) - i
			}
			if params, ok := strings.CutPrefix(s[i+2:i+end], "8;"); ok {
				_, link, _ = strings.Cut(params, ";")
			}
			i += end + 2
			continue
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				if params := s[i+2 : j]; params == "" || params == "0" {
					style = ""
				} else {
					style += s[i : j+1]
				}
			}
			i = j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		out = append(out, styledRune{r, style, link})
		i += size
	}
	return out
}

// renderStyled writes runes back as text, setting each change of style and link and resetting
// and closing the link at the end, so every line stands on its own.
func renderStyled(rs []styledRune) string {
	var b strings.Builder
	cur, link := "", ""
	for _, c := range rs {
		if c.link != link {
			if link != "" {
				b.WriteString(closeLink)
			}
			if c.link != "" {
				b.WriteString("\x1b]8;;" + c.link + "\x1b\\")
			}
			link = c.link
		}
		if c.style != cur {
			if cur != "" {
				b.WriteString(theme.Reset())
			}
			b.WriteString(c.style)
			cur = c.style
		}
		b.WriteRune(c.r)
	}
	if cur != "" {
		b.WriteString(theme.Reset())
	}
	if link != "" {
		b.WriteString(closeLink)
	}
	return b.String()
}

// closeLink ends an OSC 8 link.
const closeLink = "\x1b]8;;\x1b\\"

// inLinkedWord reports whether the word that rs[i] is in, between spaces, has a rune of a link.
func inLinkedWord(rs []styledRune, i int) bool {
	for j := i; j >= 0 && rs[j].r != ' '; j-- {
		if rs[j].link != "" {
			return true
		}
	}
	for j := i + 1; j < len(rs) && rs[j].r != ' '; j++ {
		if rs[j].link != "" {
			return true
		}
	}
	return false
}

// wrapStyled breaks styled text into lines of at most first columns, then rest, at spaces; a word
// longer than a line is cut, unless it is in a link, which the terminal could no longer open
// whole: that word is left on a line of its own, wider than the rest. The spaces at a break are dropped, and each line carries the styles
// in effect, so bold that is broken across lines stays bold on both.
func wrapStyled(s string, first, rest int) []string {
	rs := parseStyled(s)
	var lines []string
	for width := first; ; width = rest {
		w, cut, seenWord := 0, -1, false
		i := 0
		for ; i < len(rs); i++ {
			if rs[i].r == ' ' && seenWord {
				cut = i
			}
			rw := runeWidth(rs[i].r)
			if w+rw > width {
				break
			}
			w += rw
			if rs[i].r != ' ' {
				seenWord = true
			}
		}
		if i == len(rs) {
			return append(lines, renderStyled(rs))
		}
		end := cut
		if rs[i].r == ' ' && seenWord {
			end = i
		}
		if end <= 0 && inLinkedWord(rs, i) {
			for end = i; end < len(rs) && rs[end].r != ' '; end++ {
			}
		}
		if end <= 0 {
			end = max(i, 1) // no space to break at: cut the word
		}
		next := end
		for end > 0 && rs[end-1].r == ' ' {
			end--
		}
		lines = append(lines, renderStyled(rs[:end]))
		for next < len(rs) && rs[next].r == ' ' {
			next++
		}
		rs = rs[next:]
		if len(rs) == 0 {
			return lines
		}
	}
}
