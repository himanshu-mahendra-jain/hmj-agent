package theme

import (
	"fmt"
	"strings"
)

// --- Streaming styles -------------------------------------------------------

// Thinking opens a streamed reasoning region; pair with Reset().
func Thinking() string { return Raw(pal.ThinkingParam...) }

func Title(s string) string       { return Style(s, pal.Title...) }
func Muted(s string) string       { return Style(s, pal.Muted...) }
func Faint(s string) string       { return Style(s, pal.Faint...) }
func Prompt(s string) string      { return Style(s, pal.Prompt...) }
func User(s string) string        { return Style(s, pal.User...) }
func Accent(s string) string      { return Style(s, pal.Accent...) }
func FilePath(s string) string    { return Style(s, pal.FilePath...) }
func Added(s string) string       { return Style(s, pal.Added...) }
func Removed(s string) string     { return Style(s, pal.Removed...) }
func Info(s string) string        { return Style(s, pal.Info...) }
func Warn(s string) string        { return Style(s, pal.Warn...) }
func Mark(s string) string        { return Style(s, pal.SelBG...) }
func Bold(s string) string        { return Style(s, paramBold) }
func Text(s string) string        { return Style(s, pal.Text...) }
func BrightWhite(s string) string { return Style(s, pal.Text...) }

// FooterJoin renders the dim " | " separator used between status fields.
func FooterJoin() string { return Style(" | ", pal.Faint...) }

// Cached is the muted-green treatment for "N cached" in the token footer.
func Cached(s string) string { return Style(s, pal.GlyphAdd...) }

// Steps renders "(n steps)".
func Steps(n int) string { return Muted(fmt.Sprintf("(%d steps)", n)) }

// TruncationNotice is the yellow warning for model-limit cut-offs.
func TruncationNotice(s string) string { return Caution(s) }

// Rule returns a horizontal rule of the given width, styled as faint chrome.
func Rule(width int) string {
	if width < 0 {
		width = 0
	}
	if Accessibility() {
		return Faint(strings.Repeat("-", width))
	}
	return Faint(strings.Repeat("─", width))
}

// MarkdownStyle is the set of styles a run of the model's Markdown text is shown in.
type MarkdownStyle uint8

const (
	MarkdownBold MarkdownStyle = 1 << iota
	MarkdownItalic
	MarkdownCode
	MarkdownHeading
	MarkdownLink
)

// Markdown styles one run of the model's Markdown text. Every style the run is in is set in one
// sequence, so a run inside another, such as code in bold, keeps the outer style.
func Markdown(s string, st MarkdownStyle) string {
	var codes []string
	if st&MarkdownHeading != 0 {
		codes = append(codes, pal.Title...)
	}
	if st&MarkdownBold != 0 {
		codes = append(codes, paramBold)
	}
	if st&MarkdownItalic != 0 {
		codes = append(codes, paramItalic)
	}
	if st&MarkdownCode != 0 {
		codes = append(codes, pal.Accent...)
	}
	if st&MarkdownLink != 0 {
		codes = append(codes, paramUnderline)
	}
	return Style(s, codes...)
}

// Hyperlink makes s a link to url that the terminal opens when it is clicked (OSC 8), or
// returns s as it is when colour is off. Terminals without OSC 8 show s alone.
func Hyperlink(url, s string) string {
	if !enabled || url == "" || s == "" {
		return s
	}
	return "\x1b]8;;" + url + "\x1b\\" + s + "\x1b]8;;\x1b\\"
}
