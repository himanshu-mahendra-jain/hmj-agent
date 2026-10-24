// Package theme centralises every ANSI SGR sequence the agent prints.
//
// Design goals:
//   - One semantic name per visual role, so the palette can be retuned in place.
//   - Automatic no-colour fallback: when stdout is not a TTY (pipes, files,
//     `go test`, `less`, CI) or NO_COLOR is set, every style helper degrades to
//     the plain string. Callers can therefore do all layout maths on the exact
//     text they pass in and never count escape bytes.
//   - Explicit reset helpers for content we do not control (model prose, file
//     contents, command output): a stray escape sequence from that text can
//     otherwise bleed into and wreck everything printed afterwards.
package theme

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// SGR parameter codes.
const (
	resetParam     = "0"
	paramBold      = "1"
	paramDim       = "2"
	paramItalic    = "3"
	paramUnderline = "4"
	paramReverse   = "7"

	// 256-color palette codes matching s.png (dark scheme)
	cBlue      = "38;5;111" // #87afff - prompt, user label, accent, link url
	cText      = "38;5;189" // #d7d7ff - default text, diff context, messages, args
	cGreen     = "38;5;114" // #87d787 - added text, prose / assistant text
	cRed       = "38;5;210" // #ff8787 - removed text, errors
	cYellow    = "38;5;221" // #ffd75f - tool calls
	cOrange    = "38;5;214" // #ffaf00 - warnings
	cPurple    = "38;5;183" // #d7afff - tasks / info / title
	cThinking  = "38;5;146" // #afafd7 - thought process / reasoning
	cMuted     = "38;5;109" // #87afaf - dim / muted hints
	cLineNum   = "38;5;102" // #878787 - diff line numbers / faint
	cDarkRedBG = "48;5;52"  // #5f0000 - removed line background
	cDarkBG    = "48;5;234" // #1c1c1c - added line background
)

// Palette maps each semantic role onto one or more SGR codes.
type Palette struct {
	Title    []string // banner
	Muted    []string // metadata, footers, hints
	Faint    []string // deep de-emphasis: rules, gutter separators
	Prompt   []string // the user prompt marker
	User     []string // replayed user messages
	Accent   []string // selection markers, current model
	Tool     []string // tool-call header (⚙ edit_file)
	FilePath []string // paths inside tool headers
	GlyphAdd []string
	MarkAdd  []string // the "+" diff marker (bold in every palette)
	MarkDel  []string // the "-" diff marker (bold in every palette)
	Text     []string // foreground for streamed/verbatim text
	Added    []string
	Removed  []string
	Info     []string
	Warn     []string
	Error    []string
	DiffRule []string // diff header underline rule
	SelBG    []string // reverse-video emphasis inside removed/added lines

	ThinkingParam []string // streamed model reasoning style

	removedBlock []string
	addedRow     rowStyle
	removedRow   rowStyle
}

// rowStyle paints a whole diff row: the background and gutter number codes, the codes
// opening the row body (empty means the body inherits the marker's colour), and the codes
// restored after any reset inside the body.
type rowStyle struct {
	bg   []string
	num  []string
	body []string
	text []string
}

// DefaultPalette is the exact dark color scheme matching s.png.
func DefaultPalette() Palette {
	return Palette{
		Title:    []string{paramBold, cPurple},
		Muted:    []string{cMuted},
		Faint:    []string{cLineNum},
		Prompt:   []string{cBlue},
		User:     []string{cBlue},
		Accent:   []string{cBlue},
		Tool:     []string{cYellow},
		FilePath: []string{paramUnderline, cBlue},
		GlyphAdd: []string{cGreen},
		MarkAdd:  []string{paramBold, cGreen},
		MarkDel:  []string{paramBold, cRed},
		Text:     []string{cText},
		Added:    []string{cGreen},
		Removed:  []string{cRed},
		Info:     []string{cPurple},
		Warn:     []string{cOrange},
		Error:    []string{cRed},
		DiffRule: []string{cLineNum},
		SelBG:    []string{paramReverse},

		ThinkingParam: []string{paramItalic, cThinking},

		removedBlock: []string{cRed, cDarkRedBG},
		addedRow:     rowStyle{bg: []string{cDarkBG}, num: []string{cLineNum}, text: []string{cGreen}},
		removedRow:   rowStyle{bg: []string{cDarkRedBG}, num: []string{cLineNum}, text: []string{cRed}},
	}
}

var (
	pal     = selectPalette()
	enabled = computeEnabled()
)

// selectPalette returns the palette for the current high-contrast setting.
func selectPalette() Palette {
	if HighContrast() {
		return HighContrastPalette()
	}
	return DefaultPalette()
}

func computeEnabled() bool {
	return colorWanted(os.Getenv, IsTerminal(os.Stdout.Fd()))
}

// colorWanted decides colour from the environment and whether stdout is a terminal. Any
// non-empty NO_COLOR turns it off (https://no-color.org), even "0". Otherwise a non-empty
// FORCE_COLOR (https://force-color.org) or a CLICOLOR_FORCE other than "0" turns it on, also
// for a pipe or a dumb terminal; CLICOLOR=0 or TERM=dumb turn it off; and a terminal has it.
func colorWanted(getenv func(string) string, terminal bool) bool {
	switch {
	case getenv("NO_COLOR") != "":
		return false
	case getenv("FORCE_COLOR") != "", getenv("CLICOLOR_FORCE") != "" && getenv("CLICOLOR_FORCE") != "0":
		return true
	case getenv("CLICOLOR") == "0", strings.EqualFold(getenv("TERM"), "dumb"):
		return false
	}
	return terminal
}

// IsTerminal reports whether fd refers to a terminal (character device).
func IsTerminal(fd uintptr) bool {
	return term.IsTerminal(int(fd))
}

// Enabled reports whether styling helpers currently emit escape codes.
func Enabled() bool { return enabled }

// SetEnabled forces colour on or off. Exposed for tests.
func SetEnabled(on bool) { enabled = on }

// Style wraps s with the given SGR codes. Empty strings stay empty so callers
// can style optional segments without emitting a bare reset sequence.
func Style(s string, codes ...string) string {
	if s == "" || len(codes) == 0 {
		return s
	}
	if !enabled {
		return s
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + s + "\x1b[" + resetParam + "m"
}

// Raw returns a bare SGR sequence (empty when colour is off) for regions that
// span multiple writes, e.g. streamed model text. Pair it with Reset().
func Raw(codes ...string) string {
	if !enabled || len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// Reset closes a Raw() region (empty when colour is off).
func Reset() string {
	if !enabled {
		return ""
	}
	return "\x1b[" + resetParam + "m"
}

// sgr joins codes into one SGR sequence, or returns "" when there are none. It does not
// consult enabled: callers decide whether to emit colour.
func sgr(codes ...string) string {
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}
