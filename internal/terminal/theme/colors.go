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
func Tool(s string) string        { return Style(s, pal.Tool...) }
func FilePath(s string) string    { return Style(s, pal.FilePath...) }
func Added(s string) string       { return Style(s, pal.Added...) }
func Removed(s string) string     { return Style(s, pal.Removed...) }
func Modified(s string) string    { return Style(s, pal.Modified...) }
func Info(s string) string        { return Style(s, pal.Info...) }
func Warn(s string) string        { return Style(s, pal.Warn...) }
func Error(s string) string       { return Style(s, pal.Error...) }
func Highlight(s string) string   { return Style(s, pal.Highlight...) }
func Mark(s string) string        { return Style(s, pal.SelBG...) }
func Bold(s string) string        { return Style(s, paramBold) }
func Text(s string) string        { return Style(s, pal.Text...) }
func BrightWhite(s string) string { return Style(s, pal.Text...) }

func Link(url string) string     { return Style(url, pal.Link...) }
func AccentIcon(s string) string { return Style(ASCII("★ ")+s, pal.Accent...) }

// FooterJoin renders the dim " | " separator used between status fields.
func FooterJoin() string { return Style(" | ", pal.Faint...) }

// Cached is the muted-green treatment for "N cached" in the token footer.
func Cached(s string) string { return Style(s, pal.GlyphAdd...) }

// FooterDim wraps a whole footer line in muted chrome.
func FooterDim(s string) string { return Style(s, pal.Muted...) }

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
