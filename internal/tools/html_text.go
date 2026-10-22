package tools

import (
	"fmt"
	"html"
	"strings"
)

// hiddenElements hold no readable text; their whole content is skipped.
var hiddenElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "template": true,
}

// hiddenWithPageChrome adds the elements around a page's content, its site navigation, footer
// and sidebars, for a page that does not mark its content with <main> or <article>.
var hiddenWithPageChrome = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "template": true,
	"nav": true, "footer": true, "aside": true,
}

// blockSeparators are written for the start and end tags of block elements so their
// text does not run into the neighbouring text. A single "\n" is only written when the
// output does not already end a line.
var blockSeparators = map[string]string{
	"p": "\n\n", "blockquote": "\n\n",
	"div": "\n", "section": "\n", "article": "\n", "header": "\n", "footer": "\n",
	"main": "\n", "nav": "\n", "aside": "\n", "form": "\n", "title": "\n",
	"ul": "\n", "ol": "\n", "dl": "\n", "dt": "\n", "dd": "\n",
	"table": "\n", "tr": "\n",
	"figure": "\n", "figcaption": "\n",
}

// htmlConverter accumulates the text of an HTML document as Markdown-like output.
type htmlConverter struct {
	out      []byte
	hidden   map[string]bool // elements whose content is skipped
	linkHref string
	linkAt   int // offset in out where the open link's text starts, or -1
}

// htmlToText converts an HTML document to readable text: only its <main> element, or its
// <article> elements when it has no <main>, after the page's title. A page with neither
// is converted whole, without its navigation, footer and sidebars.
func htmlToText(rawHTML string) string {
	if content, title := mainContent(rawHTML); content != "" {
		if text := convertHTML(content, hiddenElements); text != "" {
			if title = collapseBlankLines(html.UnescapeString(title)); title != "" {
				return title + "\n" + text
			}
			return text
		}
	}
	return convertHTML(rawHTML, hiddenWithPageChrome)
}

// mainContent returns the markup of the document's <main> element, or the span from its first
// <article> to the end of its last, and the raw text of its <title>. content is "" when the
// document has neither element.
func mainContent(rawHTML string) (content, title string) {
	mainStart, mainEnd, articleStart, articleEnd, titleStart := -1, -1, -1, -1, -1
	scanTags(rawHTML, func(name string, closing bool, start, end int) {
		switch {
		case name == "main" && !closing && mainStart < 0:
			mainStart = start
		case name == "main" && closing && mainStart >= 0 && mainEnd < 0:
			mainEnd = end
		case name == "article" && !closing && articleStart < 0:
			articleStart = start
		case name == "article" && closing && articleStart >= 0:
			articleEnd = end
		case name == "title" && !closing && titleStart < 0:
			titleStart = end
		case name == "title" && closing && titleStart >= 0 && title == "":
			title = rawHTML[titleStart:start]
		}
	})
	switch {
	case mainStart >= 0:
		if mainEnd < 0 {
			mainEnd = len(rawHTML)
		}
		return rawHTML[mainStart:mainEnd], title
	case articleStart >= 0:
		if articleEnd < 0 {
			articleEnd = len(rawHTML)
		}
		return rawHTML[articleStart:articleEnd], title
	}
	return "", title
}

// scanTags calls visit for each start and end tag of the document, with its lower-case name
// and the offsets of its '<' and just past its '>'. Comments and the content of hidden
// elements are skipped, as the converter skips them, so markup quoted in a script never counts.
func scanTags(s string, visit func(name string, closing bool, start, end int)) {
	for pos := 0; pos < len(s); {
		lt := strings.IndexByte(s[pos:], '<')
		if lt < 0 {
			return
		}
		pos += lt
		rest := s[pos:]
		if strings.HasPrefix(rest, "<!--") {
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return
			}
			pos += 4 + end + 3
			continue
		}
		if len(rest) < 2 || !(isASCIILetter(rest[1]) || rest[1] == '/') {
			pos++
			continue
		}
		end := tagEnd(rest)
		if end < 0 {
			return
		}
		tag := rest[1:end]
		closing := strings.HasPrefix(tag, "/")
		name, attrs := splitTagName(strings.TrimPrefix(tag, "/"))
		name = strings.ToLower(name)
		visit(name, closing, pos, pos+end+1)
		pos += end + 1
		if !closing && hiddenElements[name] && !strings.HasSuffix(attrs, "/") {
			pos = len(s) - len(skipPastEndTag(s[pos:], name))
		}
	}
}

// convertHTML converts HTML to readable text in a single pass: elements in hidden and
// comments are dropped, headings, list items, rules and preformatted blocks get Markdown
// markers, links become [text](href), and whitespace-only lines collapse to at most one
// blank line.
func convertHTML(rawHTML string, hidden map[string]bool) string {
	c := htmlConverter{out: make([]byte, 0, len(rawHTML)/2), hidden: hidden, linkAt: -1}
	for s := rawHTML; s != ""; {
		lt := strings.IndexByte(s, '<')
		if lt < 0 {
			c.text(s)
			break
		}
		c.text(s[:lt])
		s = c.markup(s[lt:])
	}
	return collapseBlankLines(string(c.out))
}

func (c *htmlConverter) text(s string) {
	c.out = append(c.out, html.UnescapeString(s)...)
}

// markup consumes the markup at the start of s, which begins with '<', and returns the
// rest of the document. A '<' that cannot start a tag is kept as text, as browsers do.
func (c *htmlConverter) markup(s string) string {
	if strings.HasPrefix(s, "<!--") {
		if end := strings.Index(s[4:], "-->"); end >= 0 {
			return s[4+end+3:]
		}
		return ""
	}
	if len(s) < 2 || !(isASCIILetter(s[1]) || s[1] == '/' || s[1] == '!' || s[1] == '?') {
		c.text("<")
		return s[1:]
	}
	end := tagEnd(s)
	if end < 0 {
		return "" // an unterminated tag runs to the end of the document
	}
	tag, rest := s[1:end], s[end+1:]
	closing := strings.HasPrefix(tag, "/")
	name, attrs := splitTagName(strings.TrimPrefix(tag, "/"))
	name = strings.ToLower(name)
	if !closing && c.hidden[name] && !strings.HasSuffix(attrs, "/") {
		return skipPastEndTag(rest, name)
	}
	c.tag(name, attrs, closing)
	return rest
}

func (c *htmlConverter) tag(name, attrs string, closing bool) {
	switch name {
	case "a":
		c.endLink()
		if href := strings.TrimSpace(attrValue(attrs, "href")); !closing && href != "" {
			c.linkHref, c.linkAt = href, len(c.out)
		}
	case "br":
		c.out = append(c.out, '\n')
	case "hr":
		c.out = append(c.out, "\n---\n"...)
	case "li":
		if !closing {
			c.out = append(c.out, "\n* "...)
		}
	case "td", "th":
		if !closing {
			c.out = append(c.out, ' ')
		}
	case "pre":
		c.out = append(c.out, "\n```\n"...)
	case "h1", "h2", "h3", "h4", "h5", "h6":
		c.out = append(c.out, "\n\n"...)
		if !closing {
			c.out = append(c.out, "######"[:name[1]-'0']...)
			c.out = append(c.out, ' ')
		}
	default:
		sep := blockSeparators[name]
		if sep == "\n" && (len(c.out) == 0 || c.out[len(c.out)-1] == '\n') {
			return
		}
		c.out = append(c.out, sep...)
	}
}

// droppedLinkSchemes are link targets that are no use as text: script URLs only work in a
// browser, and data: URLs embed whole files that would bloat the output.
var droppedLinkSchemes = []string{"javascript:", "vbscript:", "data:"}

// endLink replaces the text of the open link, if any, with [text](href). Links with no
// text or a target in droppedLinkSchemes are dropped.
func (c *htmlConverter) endLink() {
	if c.linkAt < 0 {
		return
	}
	text := strings.Join(strings.Fields(string(c.out[c.linkAt:])), " ")
	c.out = c.out[:c.linkAt]
	c.linkAt = -1
	if text != "" && !hasDroppedLinkScheme(c.linkHref) {
		c.out = fmt.Appendf(c.out, "[%s](%s)", text, c.linkHref)
	}
}

func hasDroppedLinkScheme(href string) bool {
	for _, scheme := range droppedLinkSchemes {
		if len(href) >= len(scheme) && strings.EqualFold(href[:len(scheme)], scheme) {
			return true
		}
	}
	return false
}

func isASCIILetter(b byte) bool {
	return 'a' <= b|0x20 && b|0x20 <= 'z'
}

// tagEnd returns the index of the '>' that ends the tag at the start of s, skipping
// quoted attribute values, or -1 if the tag is unterminated.
func tagEnd(s string) int {
	var quote byte
	afterEquals := false
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '>':
			return i
		case '"', '\'':
			if afterEquals {
				quote = ch
			}
		case ' ', '\t', '\n', '\r', '\f':
			continue // whitespace may separate '=' from its value
		}
		afterEquals = ch == '='
	}
	return -1
}

const htmlSpace = " \t\n\r\f"

// splitTagName splits the inside of a tag into its name and attribute text.
func splitTagName(tag string) (name, attrs string) {
	if i := strings.IndexAny(tag, htmlSpace+"/"); i >= 0 {
		return tag[:i], tag[i:]
	}
	return tag, ""
}

// skipPastEndTag returns s after the end tag of the named element, or "" if the element
// is never closed.
func skipPastEndTag(s, name string) string {
	for i := 0; ; {
		j := strings.Index(s[i:], "</")
		if j < 0 {
			return ""
		}
		i += j + 2
		after := i + len(name)
		if after <= len(s) && strings.EqualFold(s[i:after], name) &&
			(after == len(s) || strings.IndexByte(htmlSpace+"/>", s[after]) >= 0) {
			if end := strings.IndexByte(s[after:], '>'); end >= 0 {
				return s[after+end+1:]
			}
			return ""
		}
	}
}

// attrValue returns the unescaped value of the named attribute in a tag's attribute
// text, or "" if it is absent. Values may be double-quoted, single-quoted or bare.
func attrValue(attrs, name string) string {
	for {
		attrs = strings.TrimLeft(attrs, htmlSpace+"/")
		if attrs == "" {
			return ""
		}
		end := strings.IndexAny(attrs, htmlSpace+"/=")
		if end < 0 {
			end = len(attrs)
		}
		key := attrs[:end]
		attrs = strings.TrimLeft(attrs[end:], htmlSpace)
		var val string
		if strings.HasPrefix(attrs, "=") {
			val, attrs = cutAttrValue(strings.TrimLeft(attrs[1:], htmlSpace))
		}
		if strings.EqualFold(key, name) {
			return html.UnescapeString(val)
		}
	}
}

// cutAttrValue splits an attribute value from the attribute text that follows it.
func cutAttrValue(s string) (val, rest string) {
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			return s[1 : 1+end], s[2+end:]
		}
		return s[1:], ""
	}
	if end := strings.IndexAny(s, htmlSpace); end >= 0 {
		return s[:end], s[end:]
	}
	return s, ""
}

// collapseBlankLines trims every line and keeps at most one blank line between lines
// of text.
func collapseBlankLines(s string) string {
	var cleaned strings.Builder
	cleaned.Grow(len(s))
	blankCount := 0
	for line := range strings.SplitSeq(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			blankCount++
			continue
		}
		if cleaned.Len() > 0 {
			cleaned.WriteByte('\n')
			if blankCount > 0 {
				cleaned.WriteByte('\n')
			}
		}
		blankCount = 0
		cleaned.WriteString(trimmed)
	}
	return cleaned.String()
}
