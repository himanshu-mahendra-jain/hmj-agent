package terminal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"hmj-agent/internal/terminal/theme"
)

var (
	origState *term.State
	termiosMu sync.Mutex
)

func SaveOrigTermios() {
	termiosMu.Lock()
	defer termiosMu.Unlock()
	if origState != nil {
		return
	}
	fd := os.Stdin.Fd()
	if isTerminal(fd) {
		if st, err := term.GetState(int(fd)); err == nil {
			origState = st
		}
	}
}

// DisableInterruptKey stops the terminal from turning Ctrl+C into SIGINT, which quits hmj.
// The prompt, the menus and the halt listener each turn it off while they read keys, but in
// the gaps between them (saving the session after a turn, drawing the next prompt) the
// terminal is back in its normal mode, and a Ctrl+C pressed then would quit. With the key
// off for the whole session, such a Ctrl+C stays queued for whatever reads keys next.
// Restore turns it back on. Accessibility mode reads lines in the terminal's own line
// editor, where Ctrl+C keeps its usual meaning, so it is left alone there.
func DisableInterruptKey() {
	termiosMu.Lock()
	defer termiosMu.Unlock()
	fd := int(os.Stdin.Fd())
	if theme.Accessibility() || !isTerminal(uintptr(fd)) {
		return
	}
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return
	}
	tio.Cc[unix.VINTR] = 0
	_ = unix.IoctlSetTermios(fd, unix.TCSETS, tio)
}

func Restore() {
	termiosMu.Lock()
	defer termiosMu.Unlock()
	DisableBracketedPaste()
	if isTerminal(os.Stdout.Fd()) {
		os.Stdout.WriteString("\x1b[?25h")
	}
	if origState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), origState)
	}
}

// isTerminal reports whether fd is a terminal. See cachedIsTerminal.
func isTerminal(fd uintptr) bool {
	return cachedIsTerminal(fd)
}

// The standard descriptors as they were when the process started. They are captured at init, before
// any code (or test) swaps os.Stdout or os.Stdin for another file.
var (
	stdinFd  = os.Stdin.Fd()
	stdoutFd = os.Stdout.Fd()

	stdinTerminal  = sync.OnceValue(func() bool { return theme.IsTerminal(stdinFd) })
	stdoutTerminal = sync.OnceValue(func() bool { return theme.IsTerminal(stdoutFd) })
)

// cachedIsTerminal returns theme.IsTerminal(fd). The answer for the standard input and output is
// computed once: spinner frames and prompt renders ask on every tick, and a terminal does not
// change while the process runs. Other descriptors are checked directly, since a descriptor number
// is reused after it is closed and cannot be memoized safely.
func cachedIsTerminal(fd uintptr) bool {
	switch fd {
	case stdinFd:
		return stdinTerminal()
	case stdoutFd:
		return stdoutTerminal()
	default:
		return theme.IsTerminal(fd)
	}
}

func DisableBracketedPaste() {
	if isTerminal(os.Stdout.Fd()) {
		os.Stdout.WriteString("\x1b[?2004l")
	}
}

func EnableBracketedPaste() {
	if isTerminal(os.Stdout.Fd()) {
		os.Stdout.WriteString("\x1b[?2004h")
	}
}

// Semantic prompt marks (OSC 133). Terminals with shell integration, such as kitty, Ghostty,
// WezTerm and iTerm2, use them to find the prompt: clicking inside it can move the cursor
// (the terminal sends arrow keys), and prompts and replies can be jumped between or selected.
// Other terminals ignore them.
const (
	markPromptStart    = "\x1b]133;A\x07"
	markSecondaryStart = "\x1b]133;A;k=s\x07" // a continuation line of a multi-line prompt
	markInputStart     = "\x1b]133;B\x07"
	markOutputStart    = "\x1b]133;C\x07"
)

// promptMarksEnabled reports whether to write OSC 133 marks: only to a real terminal, not in
// accessibility mode (its line-mode input would receive click-to-move arrows as text), and
// not on the Linux console or a dumb terminal.
func promptMarksEnabled() bool {
	switch os.Getenv("TERM") {
	case "linux", "dumb":
		return false
	}
	return !theme.Accessibility() && isTerminal(os.Stdout.Fd())
}

// markedPrompt wraps a prompt prefix in OSC 133 marks when they are enabled: the prompt
// start before it and the input start after it. secondary marks a continuation line.
func markedPrompt(prefix string, secondary bool) string {
	if !promptMarksEnabled() {
		return prefix
	}
	start := markPromptStart
	if secondary {
		start = markSecondaryStart
	}
	return start + prefix + markInputStart
}

// PromptPrefix returns the styled prompt glyph that starts every prompt, with its OSC 133
// marks when they are enabled.
func PromptPrefix() string {
	return markedPrompt(theme.Prompt("> "), false)
}

type ShortcutAction int

const (
	ActionNone ShortcutAction = iota
	ActionNew
	ActionResume
	ActionModels
	ActionHistory
)

// UnquoteDragPath normalises file paths inserted via drag-and-drop.
func UnquoteDragPath(s string) string { return unquoteDragPath(s) }

// unquoteDragPath normalises file paths inserted via drag-and-drop.
// Different sources format paths differently:
//   - Most terminals: single-quoted  '/path/to/file'
//   - Some terminals: double-quoted  "/path/to/file"
//   - Some terminals: backslash-escaped  /path/to/my\ file
//   - ChromeOS Files app: file:///path/to/file  (URL-encoded)
func unquoteDragPath(s string) string {
	s = strings.TrimSpace(s)
	// file:// URI (ChromeOS Files, some other drag sources)
	if after, ok := strings.CutPrefix(s, "file://"); ok {
		path := after
		// Strip optional hostname (file://localhost/path or file:///path)
		if len(path) > 0 && path[0] != '/' {
			if idx := strings.Index(path, "/"); idx >= 0 {
				path = path[idx:]
			}
		}
		// Percent-decode the path
		path = percentDecode(path)
		// Trim trailing newline/space that some apps append
		path = strings.TrimRight(path, "\r\n ")
		return path
	}
	// Single-quoted: '/some/path'
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		inner := s[1 : len(s)-1]
		if !strings.Contains(inner, "'") {
			return inner
		}
	}
	// Double-quoted: "/some/path"
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		if !strings.Contains(inner, "\"") {
			return inner
		}
	}
	// Backslash-escaped spaces: /path/to/my\ file
	if strings.HasPrefix(s, "/") && strings.Contains(s, "\\ ") {
		return strings.ReplaceAll(s, "\\ ", " ")
	}
	return s
}

// percentDecode decodes URL percent-encoding (%XX) in a path string.
func percentDecode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) {
			hi := hexVal(s[i+1])
			lo := hexVal(s[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// ReadSeamlessInput reads input interactively from the terminal.
// Multi-line pastes are rendered across lines on the screen and do NOT send the prompt
// immediately; the prompt is only sent when the user presses Enter on their keyboard.
// In accessibility mode it reads whole lines instead (see readAccessibleInput).
func ReadSeamlessInput(reader *bufio.Reader) (string, ShortcutAction, error) {
	if theme.Accessibility() {
		return readAccessibleInput(reader)
	}

	fd := int(os.Stdin.Fd())

	// Non-terminal fallback (tests, pipes)
	if !isTerminal(uintptr(fd)) {
		return readPipedInput(reader)
	}

	// Drain any residual bytes in reader buffer
	initial := drainBuffered(reader)

	oldTermios, termErr := unix.IoctlGetTermios(fd, unix.TCGETS)
	if termErr != nil {
		line, err := reader.ReadString('\n')
		return CleanInput(line), ActionNone, err
	}

	// Put terminal in raw/cbreak mode
	raw := *oldTermios
	raw.Lflag &^= (unix.ICANON | unix.ECHO)
	// Read carriage returns as they are sent, so a pasted CRLF line ending reaches
	// finishPaste as one line break instead of being turned into two newlines. Enter
	// (a carriage return) still submits. Ctrl+S is an ignored key rather than XOFF, which
	// would stop all output, and hmj with it, until Ctrl+Q.
	raw.Iflag &^= unix.ICRNL | unix.IXON
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	// Deliver Ctrl+C as a key (handled in-app) instead of raising SIGINT, which
	// would quit the agent. Other signal keys such as Ctrl+Z keep working.
	raw.Cc[unix.VINTR] = 0

	_ = unix.IoctlSetTermios(fd, unix.TCSETS, &raw)
	EnableBracketedPaste()

	defer func() {
		DisableBracketedPaste()
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, oldTermios)
	}()

	s := &seamlessInput{fd: fd, reader: reader, ed: NewPromptEditor(initial)}
	return s.run()
}

// seamlessInput is the state of one ReadSeamlessInput call while the terminal is in raw mode.
type seamlessInput struct {
	fd       int
	reader   *bufio.Reader
	ed       *PromptEditor
	inPaste  bool
	pasteBuf strings.Builder
	pending  []byte // bytes decodeRune read past a cut-short character, handled next
}

// run reads keys until the input ends, then returns the text entered so far.
func (s *seamlessInput) run() (string, ShortcutAction, error) {
	b := make([]byte, 1)
	for {
		if len(s.pending) > 0 {
			b[0], s.pending = s.pending[0], s.pending[1:]
		} else if n, err := os.Stdin.Read(b); err != nil || n == 0 {
			break
		}
		if done, text, action, keyErr := s.handleKey(b[0]); done {
			return text, action, keyErr
		}
		if s.ed.stale && len(s.pending) == 0 && !s.inputQueued() {
			s.ed.Render()
		}
	}
	return CleanInput(string(s.ed.runes)), ActionNone, nil
}

// handleKey applies one byte of input. It reports done=true when the input has ended, with
// the submitted text, a shortcut action or an error.
func (s *seamlessInput) handleKey(ch byte) (done bool, text string, action ShortcutAction, err error) {
	// Control keys act on what is on screen, so draw any held-back text first.
	if s.ed.stale && (ch < 0x20 || ch == 0x7f) && ch != '\r' && ch != '\n' {
		s.ed.Render()
	}
	if ch == 0x1b {
		s.handleEscape()
		return false, "", ActionNone, nil
	}

	// Inside bracketed paste: collected as is, cleaned up by finishPaste.
	if s.inPaste {
		s.pasteBuf.WriteByte(ch)
		return false, "", ActionNone, nil
	}

	switch ch {
	case 0x03: // Ctrl+C clears the typed text in place; on an empty prompt it does nothing.
		// It never ends the input or quits: Ctrl+D does that.
		if len(s.ed.runes) > 0 {
			s.ed.Clear()
		}
		return false, "", ActionNone, nil
	case 0x04: // Ctrl+D: exit HMJ Agent
		os.Stdout.WriteString("\n")
		return true, "", ActionNone, io.EOF
	case 0x0e: // Ctrl+N: new conversation
		return s.endWithAction(ActionNew)
	case 0x12: // Ctrl+R: resume conversation
		return s.endWithAction(ActionResume)
	case 0x0f: // Ctrl+O: switch model
		return s.endWithAction(ActionModels)
	case 0x19: // Ctrl+Y: turn history and copy
		return s.endWithAction(ActionHistory)
	case 0x01: // Ctrl+A: start of line
		s.ed.MoveLineStart()
	case 0x05: // Ctrl+E: end of line
		s.ed.MoveLineEnd()
	case 0x15: // Ctrl+U: delete to start of line
		s.ed.DeleteToLineStart()
	case 0x0b: // Ctrl+K: delete to end of line
		s.ed.DeleteToLineEnd()
	case 0x17: // Ctrl+W: delete previous word
		s.ed.DeleteWordBackward()
	case 0x7f, 0x08: // Backspace
		s.ed.Backspace()
	case '\r', '\n': // Enter key
		return s.submit()
	default:
		// Unbound control keys (Ctrl+B, Ctrl+L, ...) are ignored rather than typed into the
		// prompt as invisible characters. Tab is kept; pasted text never reaches this switch.
		if ch < 0x20 && ch != '\t' {
			return false, "", ActionNone, nil
		}
		r := s.decodeRune(ch)
		// RuneError from an incomplete/invalid sequence is dropped; no separate error path.
		// So are C1 control characters (U+0080 to U+009F): drawn, U+009B would start a
		// control sequence just like ESC [.
		if r == utf8.RuneError || (r >= 0x80 && r <= 0x9f) {
			return false, "", ActionNone, nil
		}
		if r == '/' && !s.inPaste && len(strings.TrimSpace(string(s.ed.runes))) == 0 {
			return s.openSlashMenu()
		}
		if s.inputQueued() {
			s.ed.insertWithoutRender(r)
		} else {
			s.ed.Insert(r)
		}
	}
	return false, "", ActionNone, nil
}

// inputQueued reports whether more input is already waiting to be read.
func (s *seamlessInput) inputQueued() bool {
	queued, err := hasInputPending(uintptr(s.fd), 0)
	return err == nil && queued
}

// endWithAction ends the input for a keyboard shortcut, moving past the prompt line first.
func (s *seamlessInput) endWithAction(action ShortcutAction) (bool, string, ShortcutAction, error) {
	os.Stdout.WriteString("\n")
	return true, "", action, nil
}

// submit handles Enter. When more input is already pending (a multi-line paste) the newline
// is inserted instead of submitting the prompt.
func (s *seamlessInput) submit() (bool, string, ShortcutAction, error) {
	// A poll failure is treated as "no more input pending" — the
	// subsequent Enter submit remains the safe default.
	hasMore, pollErr := hasInputPending(uintptr(s.fd), 10*time.Millisecond)
	if pollErr != nil {
		hasMore = false
	}
	if hasMore {
		s.ed.insertWithoutRender('\n')
		return false, "", ActionNone, nil
	}
	if s.ed.stale {
		s.ed.Render()
	}

	// Move cursor down to last rendered visual row before printing newline
	vl := s.ed.VisualLayout(s.ed.TerminalWidth())
	if down := (vl.TotalVisualRows - 1) - vl.CursorVisualRow; down > 0 {
		fmt.Fprintf(os.Stdout, "\x1b[%dB", down)
	}
	os.Stdout.WriteString("\n")
	if promptMarksEnabled() {
		os.Stdout.WriteString(markOutputStart)
	}
	return true, CleanInput(string(s.ed.runes)), ActionNone, nil
}

// openSlashMenu shows the command picker for a "/" typed into an empty prompt. A confirmed
// command ends the input; a cancelled one clears the prompt and keeps reading.
func (s *seamlessInput) openSlashMenu() (bool, string, ShortcutAction, error) {
	item, confirmed, err := interactiveSelect(
		"commands (type to filter, ↑/↓ navigate, Enter select, Esc cancel)",
		DefaultSlashCommands(),
		0,
		s.reader,
		nil,
		false,
	)
	if err != nil {
		return true, "", ActionNone, err
	}
	if confirmed && item.ID != "" {
		os.Stdout.WriteString(PromptPrefix() + theme.Accent(item.ID) + "\n")
		return true, item.ID, ActionNone, nil
	}
	os.Stdout.WriteString("\r\x1b[J" + PromptPrefix())
	s.ed.Clear()
	return false, "", ActionNone, nil
}

// decodeRune decodes the UTF-8 character that starts with first, reading any continuation
// bytes from stdin. It returns utf8.RuneError for an incomplete or invalid sequence. A byte
// that cannot continue the character (a Latin-1 "é" followed by Enter) is kept in pending
// and handled as the next key, so the cut-short character does not swallow it.
func (s *seamlessInput) decodeRune(first byte) rune {
	if first < utf8.RuneSelf {
		return rune(first)
	}
	utfBuf := []byte{first}
	for !utf8.FullRune(utfBuf) {
		nb := make([]byte, 1)
		n, err := os.Stdin.Read(nb)
		if err != nil || n == 0 {
			break
		}
		if !utf8.RuneStart(nb[0]) {
			utfBuf = append(utfBuf, nb[0])
			continue
		}
		s.pending = append(s.pending, nb[0])
		break
	}
	r, _ := utf8.DecodeRune(utfBuf)
	return r
}

// handleEscape reads an escape sequence after an Esc byte. Inside a paste it is kept for
// ANSI stripping; outside one it is run as a cursor or editing command.
func (s *seamlessInput) handleEscape() {
	seq := readEscapeSeq(s.fd)
	switch seq {
	case "[200~":
		s.inPaste = true
		s.pasteBuf.Reset()
		return
	case "[201~":
		s.inPaste = false
		s.finishPaste()
		return
	}

	// When inside paste, write escape sequence raw to buffer (will be stripped by stripANSI)
	if s.inPaste {
		s.pasteBuf.WriteByte(0x1b)
		s.pasteBuf.WriteString(seq)
		return
	}

	// Outside paste: execute as keyboard command
	switch seq {
	case "[A", "OA": // Up Arrow
		s.ed.MoveUp()
	case "[B", "OB": // Down Arrow
		s.ed.MoveDown()
	case "[C", "OC": // Right Arrow
		s.ed.MoveRight()
	case "[D", "OD": // Left Arrow
		s.ed.MoveLeft()
	case "[H", "[1~", "OH": // Home
		s.ed.MoveHome()
	case "[F", "[4~", "OF": // End
		s.ed.MoveEnd()
	case "[3~": // Delete key
		s.ed.Delete()
	case "[1;5D", "[1;3D", "b": // Ctrl+Left, Alt+Left, Alt+B: previous word
		s.ed.MoveWordLeft()
	case "[1;5C", "[1;3C", "f": // Ctrl+Right, Alt+Right, Alt+F: next word
		s.ed.MoveWordRight()
	case "\x7f", "\x08": // Alt+Backspace: delete the word before the cursor
		s.ed.DeleteWordBefore()
	case "d", "[3;5~", "[3;3~": // Alt+D, Ctrl+Delete, Alt+Delete: delete the word after the cursor
		s.ed.DeleteWordAfter()
	case "": // Standalone Esc: wipe prompt text
		if len(s.ed.runes) > 0 {
			s.ed.Clear()
		}
	}
}

// finishPaste inserts the collected paste text with line endings, ANSI codes, control
// characters and drag-and-drop quoting normalised.
func (s *seamlessInput) finishPaste() {
	pasted := s.pasteBuf.String()
	// Windows (CRLF) and old Mac (CR) line endings become one newline each.
	pasted = strings.ReplaceAll(pasted, "\r\n", "\n")
	pasted = strings.ReplaceAll(pasted, "\r", "\n")
	// Strip ANSI escape codes (present when copying from terminal output) and the other
	// control characters, which would move the cursor or ring the bell when drawn.
	pasted = theme.Sanitize(pasted)
	// Normalize drag-and-drop paths (shell quotes, file:// URIs, backslash escapes). Plain
	// text is kept as pasted: trimming it would eat the space in "hello" + " world" and the
	// indentation of pasted code.
	if path := UnquoteDragPath(pasted); path != strings.TrimSpace(pasted) {
		pasted = path
	}
	s.ed.InsertRunes([]rune(pasted))
	s.pasteBuf.Reset()
}

// readPipedInput reads the input when stdin is not a terminal: one line, plus any further
// lines already buffered, with the control bytes used for shortcuts mapped to their actions.
func readPipedInput(reader *bufio.Reader) (string, ShortcutAction, error) {
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", ActionNone, err
	}
	var sb strings.Builder
	sb.WriteString(line)
	for reader.Buffered() > 0 {
		more, err := reader.ReadString('\n')
		sb.WriteString(more)
		if err != nil {
			break
		}
	}
	rawStr := strings.TrimRight(sb.String(), "\r\n")
	switch rawStr {
	case "\x0e":
		return "", ActionNew, nil
	case "\x12":
		return "", ActionResume, nil
	case "\x0f":
		return "", ActionModels, nil
	case "\x19":
		return "", ActionHistory, nil
	case "/":
		return "/shortcuts", ActionNone, nil
	case "\x03", "\x1b":
		return "", ActionNone, nil
	}
	return CleanInput(sb.String()), ActionNone, nil
}

// drainBuffered removes and returns the bytes already buffered in reader.
func drainBuffered(reader *bufio.Reader) string {
	var sb strings.Builder
	for reader.Buffered() > 0 {
		b, err := reader.ReadByte()
		if err != nil {
			break
		}
		sb.WriteByte(b)
	}
	return sb.String()
}

// DefaultSlashCommands returns the list of interactive slash commands.
func DefaultSlashCommands() []SelectItem {
	return []SelectItem{
		{ID: "/new", Text: "/new        Start a new conversation (Ctrl+N)"},
		{ID: "/resume", Text: "/resume     Resume / manage conversations (Ctrl+R)"},
		{ID: "/models", Text: "/models     Switch AI model (Ctrl+O)"},
		{ID: "/health", Text: "/health     Check latency & health of all providers and models"},
		{ID: "/history", Text: "/history    Browse turns & copy prompt or AI output (Ctrl+Y)"},
		{ID: "/shortcuts", Text: "/shortcuts  Display shortcuts & commands cheatsheet table"},
	}
}

func getTerminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	if w, _, err := term.GetSize(int(os.Stdin.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

// rowWidth is the number of columns a redrawn row may use. The last column is left free so a
// full row never leaves the cursor in the terminal's pending-wrap state.
func rowWidth() int {
	return max(getTerminalWidth()-1, 1)
}

// clipEnd shortens plain text s to at most width columns, ending it with "…" when cut.
// Rows that are redrawn in place must not wrap, or moving the cursor up by the row count
// lands short and leaves a stale copy of the top rows on screen.
func clipEnd(s string, width int) string {
	if VisibleWidth(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	var sb strings.Builder
	used := 0
	for _, r := range s {
		w := runeWidth(r)
		if used+w > width-1 {
			break
		}
		sb.WriteRune(r)
		used += w
	}
	sb.WriteString("…")
	return sb.String()
}

// clipStart is clipEnd keeping the end of s, for text being typed where the cursor sits.
func clipStart(s string, width int) string {
	if VisibleWidth(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	rs := []rune(s)
	used, i := 0, len(rs)
	for i > 0 && used+runeWidth(rs[i-1]) <= width-1 {
		i--
		used += runeWidth(rs[i])
	}
	return "…" + string(rs[i:])
}

var (
	globalPromptHistory []string
	historyMu           sync.Mutex
)

// AddPromptHistory records a user prompt into the global prompt history.
func AddPromptHistory(prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	if len(globalPromptHistory) > 0 && globalPromptHistory[len(globalPromptHistory)-1] == prompt {
		return
	}
	globalPromptHistory = append(globalPromptHistory, prompt)
}

// SetPromptHistory replaces the global prompt history.
func SetPromptHistory(prompts []string) {
	historyMu.Lock()
	defer historyMu.Unlock()
	globalPromptHistory = nil
	for _, p := range prompts {
		p = strings.TrimSpace(p)
		if p != "" {
			globalPromptHistory = append(globalPromptHistory, p)
		}
	}
}

// GetPromptHistory returns a copy of the prompt history.
func GetPromptHistory() []string {
	historyMu.Lock()
	defer historyMu.Unlock()
	cp := make([]string, len(globalPromptHistory))
	copy(cp, globalPromptHistory)
	return cp
}

type PromptEditor struct {
	runes             []rune
	cursor            int
	prompt            string
	indent            string
	renderedVisualRow int // cursor's visual row offset from top of prompt (0-based)
	renderedTotalRows int // total visual rows rendered
	renderedLine      int // cursor's current line index (0-based)
	renderedLines     int // total lines rendered minus 1 (index of last rendered line)
	width             int // optional fixed width for testing or overrides; 0 = auto-detect
	historyIndex      int // 0 = current draft, 1 = 1 back, etc.
	savedDraft        string
	stale             bool // the text changed since the last Render
}

func NewPromptEditor(initial string) *PromptEditor {
	ed := &PromptEditor{
		prompt: "> ",
		indent: "  ",
	}
	if len(initial) > 0 {
		ed.runes = []rune(initial)
		ed.cursor = len(ed.runes)
		ed.Render()
	}
	return ed
}

func (e *PromptEditor) SetWidth(w int) {
	e.width = w
}

func (e *PromptEditor) TerminalWidth() int {
	if e.width > 0 {
		return e.width
	}
	return getTerminalWidth()
}

func (e *PromptEditor) Text() string {
	return string(e.runes)
}

func (e *PromptEditor) Cursor() int {
	return e.cursor
}

func (e *PromptEditor) Layout() (lines []string, curLine, curCol int) {
	s := string(e.runes)
	lines = strings.Split(s, "\n")

	pos := 0
	found := false
	for i, l := range lines {
		runeLen := len([]rune(l))
		if !found && e.cursor <= pos+runeLen {
			curLine = i
			curCol = e.cursor - pos
			found = true
		}
		pos += runeLen + 1
	}
	if !found {
		curLine = len(lines) - 1
		curCol = len([]rune(lines[len(lines)-1]))
	}
	return lines, curLine, curCol
}

type VisualLayoutInfo struct {
	Lines           []string
	CurLine         int
	CurCol          int
	CursorVisualRow int
	CursorVisualCol int
	TotalVisualRows int
}

// wrapPromptLine lays out one prompt line, prefix first, the way a terminal wraps it at width
// columns: a double-width character (CJK, most emoji) takes two columns and moves to the next
// row whole when only one column is left. It returns the number of rows the line takes and
// the row and column of the cursor when it sits before rune cursor (or at the end of the
// line). A cursor right after a full last row reports column width, as the terminal keeps it
// on that row until the next character.
func wrapPromptLine(prefix string, line []rune, cursor, width int) (rows, cursorRow, cursorCol int) {
	col := VisibleWidth(prefix)
	for i, r := range line {
		w := runeWidth(r)
		if r < 0x20 {
			w = 1 // a Tab or other control is drawn as one space (see promptDisplayLine)
		}
		if col+w > width {
			rows++
			col = 0
		}
		if i == cursor {
			cursorRow, cursorCol = rows, col
		}
		col += w
	}
	if cursor >= len(line) {
		cursorRow, cursorCol = rows, col
	}
	return rows + 1, cursorRow, cursorCol
}

func (e *PromptEditor) VisualLayout(width int) VisualLayoutInfo {
	if width <= 0 {
		width = 80
	}

	lines, curLine, curCol := e.Layout()
	var (
		totalVisualRows int
		cursorVisualRow int
		cursorVisualCol int
	)

	for i, l := range lines {
		prefix := e.indent
		if i == 0 {
			prefix = e.prompt
		}
		lineRows, rowAtCursor, colAtCursor := wrapPromptLine(prefix, []rune(l), curCol, width)

		if i < curLine {
			cursorVisualRow += lineRows
		} else if i == curLine {
			cursorVisualRow += rowAtCursor
			cursorVisualCol = colAtCursor
		}

		totalVisualRows += lineRows
	}

	if totalVisualRows < 1 {
		totalVisualRows = 1
	}

	return VisualLayoutInfo{
		Lines:           lines,
		CurLine:         curLine,
		CurCol:          curCol,
		CursorVisualRow: cursorVisualRow,
		CursorVisualCol: cursorVisualCol,
		TotalVisualRows: totalVisualRows,
	}
}

// Render redraws the prompt. The frame is written in one piece so the terminal never
// shows it half drawn.
func (e *PromptEditor) Render() {
	width := e.TerminalWidth()
	vl := e.VisualLayout(width)

	if isTerminal(os.Stdout.Fd()) {
		var frame strings.Builder
		if e.renderedVisualRow > 0 {
			fmt.Fprintf(&frame, "\x1b[%dA", e.renderedVisualRow)
		}
		frame.WriteString("\r\x1b[J")

		for i, l := range vl.Lines {
			prefix := markedPrompt(e.indent, true)
			if i == 0 {
				prefix = markedPrompt(e.prompt, false)
			}
			frame.WriteString(prefix + promptDisplayLine(l))
			if i < len(vl.Lines)-1 {
				frame.WriteString("\r\n") // \r\n ensures col 0 on next line in raw mode
			}
		}

		lastVisualRow := vl.TotalVisualRows - 1
		if up := lastVisualRow - vl.CursorVisualRow; up > 0 {
			fmt.Fprintf(&frame, "\x1b[%dA", up)
		}

		frame.WriteString("\r")
		if vl.CursorVisualCol > 0 {
			fmt.Fprintf(&frame, "\x1b[%dC", vl.CursorVisualCol)
		}
		os.Stdout.WriteString(frame.String())
	}

	e.stale = false
	e.renderedVisualRow = vl.CursorVisualRow
	e.renderedTotalRows = vl.TotalVisualRows
	e.renderedLine = vl.CurLine
	e.renderedLines = len(vl.Lines) - 1
}

// promptDisplayLine is a prompt line as Render draws it: a Tab (or any other control
// character) is drawn as one space. Written as is, a Tab would jump to the next tab stop,
// a distance that depends on the column, and the cursor would land inside the gap.
func promptDisplayLine(l string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, l)
}

func (e *PromptEditor) Clear() {
	if isTerminal(os.Stdout.Fd()) {
		if e.renderedVisualRow > 0 {
			fmt.Fprintf(os.Stdout, "\x1b[%dA", e.renderedVisualRow)
		}
		os.Stdout.WriteString("\r\x1b[J")
		os.Stdout.WriteString(markedPrompt(e.prompt, false))
	}

	e.runes = nil
	e.cursor = 0
	e.renderedVisualRow = 0
	e.renderedTotalRows = 0
	e.renderedLine = 0
	e.renderedLines = 0
}

func (e *PromptEditor) Insert(r rune) {
	e.insertWithoutRender(r)
	e.Render()
}

// insertWithoutRender inserts r like Insert but leaves the redraw to a later Render, so a
// burst of queued input (typed ahead, or pasted without bracketed paste) is drawn once
// instead of once per character.
func (e *PromptEditor) insertWithoutRender(r rune) {
	e.historyIndex = 0
	e.runes = slices.Insert(e.runes, e.cursor, r)
	e.cursor++
	e.stale = true
}

func (e *PromptEditor) InsertRunes(rs []rune) {
	if len(rs) == 0 {
		return
	}
	e.historyIndex = 0
	if e.cursor >= len(e.runes) {
		e.runes = append(e.runes, rs...)
		e.cursor = len(e.runes)
	} else {
		newRunes := make([]rune, 0, len(e.runes)+len(rs))
		newRunes = append(newRunes, e.runes[:e.cursor]...)
		newRunes = append(newRunes, rs...)
		newRunes = append(newRunes, e.runes[e.cursor:]...)
		e.runes = newRunes
		e.cursor += len(rs)
	}
	e.Render()
}

func (e *PromptEditor) Backspace() {
	if e.cursor <= 0 || len(e.runes) == 0 {
		return
	}
	e.historyIndex = 0
	e.runes = append(e.runes[:e.cursor-1], e.runes[e.cursor:]...)
	e.cursor--
	e.Render()
}

func (e *PromptEditor) Delete() {
	if e.cursor >= len(e.runes) {
		return
	}
	e.historyIndex = 0
	e.runes = append(e.runes[:e.cursor], e.runes[e.cursor+1:]...)
	e.Render()
}

func (e *PromptEditor) MoveLeft() {
	if e.cursor > 0 {
		e.cursor--
		e.Render()
	}
}

func (e *PromptEditor) MoveRight() {
	if e.cursor < len(e.runes) {
		e.cursor++
		e.Render()
	}
}

func (e *PromptEditor) MoveUp() {
	lines, curLine, curCol := e.Layout()
	if curLine > 0 {
		targetLine := curLine - 1
		targetCol := curCol
		lineLen := len([]rune(lines[targetLine]))
		if targetCol > lineLen {
			targetCol = lineLen
		}
		newPos := 0
		for _, line := range lines[:targetLine] {
			newPos += len([]rune(line)) + 1
		}
		newPos += targetCol
		e.cursor = newPos
		e.Render()
		return
	}

	// At top line: cycle backward through prompt history
	history := GetPromptHistory()
	if len(history) == 0 {
		return
	}
	if e.historyIndex == 0 {
		e.savedDraft = string(e.runes)
	}
	if e.historyIndex < len(history) {
		e.historyIndex++
		item := history[len(history)-e.historyIndex]
		e.runes = []rune(item)
		e.cursor = len(e.runes)
		e.Render()
	}
}

func (e *PromptEditor) MoveDown() {
	lines, curLine, curCol := e.Layout()
	if curLine < len(lines)-1 {
		targetLine := curLine + 1
		targetCol := curCol
		lineLen := len([]rune(lines[targetLine]))
		if targetCol > lineLen {
			targetCol = lineLen
		}
		newPos := 0
		for _, line := range lines[:targetLine] {
			newPos += len([]rune(line)) + 1
		}
		newPos += targetCol
		e.cursor = newPos
		e.Render()
		return
	}

	// At bottom line: cycle forward through prompt history
	if e.historyIndex <= 0 {
		return
	}
	e.historyIndex--
	if e.historyIndex == 0 {
		e.runes = []rune(e.savedDraft)
		e.cursor = len(e.runes)
		e.Render()
		return
	}
	history := GetPromptHistory()
	item := history[len(history)-e.historyIndex]
	e.runes = []rune(item)
	e.cursor = len(e.runes)
	e.Render()
}

func (e *PromptEditor) MoveHome() {
	lines, curLine, _ := e.Layout()
	newPos := 0
	for _, line := range lines[:curLine] {
		newPos += len([]rune(line)) + 1
	}
	e.cursor = newPos
	e.Render()
}

func (e *PromptEditor) MoveEnd() {
	lines, curLine, _ := e.Layout()
	newPos := 0
	for i, line := range lines[:curLine+1] {
		newPos += len([]rune(line))
		if i < curLine {
			newPos++
		}
	}
	e.cursor = newPos
	e.Render()
}

// lineStart returns the index of the first rune of the line holding the cursor.
func (e *PromptEditor) lineStart() int {
	i := e.cursor
	for i > 0 && e.runes[i-1] != '\n' {
		i--
	}
	return i
}

// lineEnd returns the index just past the last rune of the line holding the cursor.
func (e *PromptEditor) lineEnd() int {
	i := e.cursor
	for i < len(e.runes) && e.runes[i] != '\n' {
		i++
	}
	return i
}

// MoveLineStart moves the cursor to the start of the current line (Ctrl+A).
func (e *PromptEditor) MoveLineStart() {
	e.cursor = e.lineStart()
	e.Render()
}

// MoveLineEnd moves the cursor to the end of the current line (Ctrl+E).
func (e *PromptEditor) MoveLineEnd() {
	e.cursor = e.lineEnd()
	e.Render()
}

// deleteRange removes runes[start:end] and leaves the cursor at start.
func (e *PromptEditor) deleteRange(start, end int) {
	if start >= end {
		return
	}
	e.historyIndex = 0
	e.runes = slices.Delete(e.runes, start, end)
	e.cursor = start
	e.Render()
}

// DeleteToLineStart deletes from the start of the current line to the cursor (Ctrl+U).
func (e *PromptEditor) DeleteToLineStart() {
	e.deleteRange(e.lineStart(), e.cursor)
}

// DeleteToLineEnd deletes from the cursor to the end of the current line (Ctrl+K).
func (e *PromptEditor) DeleteToLineEnd() {
	e.deleteRange(e.cursor, e.lineEnd())
}

// DeleteWordBackward deletes trailing spaces and then the word before the
// cursor, never crossing a newline (Ctrl+W).
func (e *PromptEditor) DeleteWordBackward() {
	start := e.lineStart()
	i := e.cursor
	for i > start && unicode.IsSpace(e.runes[i-1]) {
		i--
	}
	for i > start && !unicode.IsSpace(e.runes[i-1]) {
		i--
	}
	e.deleteRange(i, e.cursor)
}

// isWordRune reports whether r belongs to a word for the Alt and Ctrl word keys: letters,
// digits and underscores, so "src/main.go" is three words, as in bash and zsh.
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// wordStartBefore returns the start of the word before pos, skipping the non-word runes
// in between. It does not go below limit.
func (e *PromptEditor) wordStartBefore(pos, limit int) int {
	for pos > limit && !isWordRune(e.runes[pos-1]) {
		pos--
	}
	for pos > limit && isWordRune(e.runes[pos-1]) {
		pos--
	}
	return pos
}

// wordEndAfter returns the end of the word after pos, skipping the non-word runes in
// between. It does not go above limit.
func (e *PromptEditor) wordEndAfter(pos, limit int) int {
	for pos < limit && !isWordRune(e.runes[pos]) {
		pos++
	}
	for pos < limit && isWordRune(e.runes[pos]) {
		pos++
	}
	return pos
}

// MoveWordLeft moves the cursor to the start of the previous word, across lines
// (Ctrl+Left, Alt+Left, Alt+B).
func (e *PromptEditor) MoveWordLeft() {
	e.cursor = e.wordStartBefore(e.cursor, 0)
	e.Render()
}

// MoveWordRight moves the cursor to the end of the next word, across lines
// (Ctrl+Right, Alt+Right, Alt+F).
func (e *PromptEditor) MoveWordRight() {
	e.cursor = e.wordEndAfter(e.cursor, len(e.runes))
	e.Render()
}

// DeleteWordBefore deletes the word before the cursor, stopping at punctuation and never
// crossing a newline (Alt+Backspace). Ctrl+W deletes back to whitespace instead.
func (e *PromptEditor) DeleteWordBefore() {
	e.deleteRange(e.wordStartBefore(e.cursor, e.lineStart()), e.cursor)
}

// DeleteWordAfter deletes the word after the cursor, never crossing a newline
// (Alt+D, Ctrl+Delete).
func (e *PromptEditor) DeleteWordAfter() {
	e.deleteRange(e.cursor, e.wordEndAfter(e.cursor, e.lineEnd()))
}

func readEscapeSeq(fd int) string {
	var seq strings.Builder
	b := make([]byte, 1)
	for {
		hasMore, pollErr := hasInputPending(uintptr(fd), 25*time.Millisecond)
		if pollErr != nil || !hasMore {
			break
		}
		n, err := os.Stdin.Read(b)
		if err != nil || n == 0 {
			break
		}
		seq.WriteByte(b[0])
		// SS3 sequences (ESC O <final>, sent in application-cursor mode) have a
		// letter as their introducer, so read on to the final byte.
		if seq.Len() == 1 && b[0] == 'O' {
			continue
		}
		if (b[0] >= 'A' && b[0] <= 'Z') || (b[0] >= 'a' && b[0] <= 'z') || b[0] == '~' {
			break
		}
		// Alt+Backspace is ESC followed by the Backspace byte; it ends there, so a held
		// Alt+Backspace is read as one key per press.
		if seq.Len() == 1 && (b[0] == 0x7f || b[0] == 0x08) {
			break
		}
	}
	return seq.String()
}

func CleanInput(s string) string {
	s = strings.ReplaceAll(s, "\x1b[200~", "")
	s = strings.ReplaceAll(s, "\x1b[201~", "")
	s = strings.ReplaceAll(s, "^[[200~", "")
	s = strings.ReplaceAll(s, "^[[201~", "")
	return strings.TrimSpace(s)
}

func hasInputPending(fd uintptr, timeout time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, int(timeout.Milliseconds()))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		return n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
	}
}

func flushStdin() {
	fd := os.Stdin.Fd()
	for {
		hasMore, err := hasInputPending(fd, 5*time.Millisecond)
		if err != nil || !hasMore {
			break
		}
		var b [256]byte
		if _, readErr := unix.Read(int(fd), b[:]); readErr != nil {
			break // read error while draining: stop flushing
		}
	}
}

// StartHaltListener listens for the Escape key (or Ctrl+C) while the agent is running.
func StartHaltListener(cancel context.CancelFunc) func() {
	fd := int(os.Stdin.Fd())
	oldTermios, termErr := unix.IoctlGetTermios(fd, unix.TCGETS)
	if termErr != nil {
		return func() {}
	}

	flushStdin()

	raw := *oldTermios
	raw.Lflag &^= (unix.ICANON | unix.ECHO | unix.ISIG)
	raw.Cc[unix.VMIN] = 0
	raw.Cc[unix.VTIME] = 0

	_ = unix.IoctlSetTermios(fd, unix.TCSETS, &raw)

	done := make(chan struct{})
	exited := make(chan struct{})

	go func() {
		defer close(exited)
		b := make([]byte, 1)
		for {
			select {
			case <-done:
				return
			default:
			}

			hasByte, pollErr := hasInputPending(uintptr(fd), 50*time.Millisecond)
			// A key that arrives once the turn has ended belongs to the next prompt.
			select {
			case <-done:
				return
			default:
			}
			if pollErr != nil || !hasByte {
				continue
			}

			n, err := os.Stdin.Read(b)
			if err != nil || n == 0 {
				return
			}

			if b[0] == 0x03 {
				StopActiveSpinner()
				fmt.Printf("\n%s\n", theme.Warn("[halted by user (Ctrl+C)]"))
				cancel()
				return
			}

			if b[0] == 0x1b {
				seq := readEscapeSeq(fd)
				if seq != "" {
					continue
				}

				StopActiveSpinner()
				fmt.Printf("\n%s\n", theme.Warn("[halted by user (Esc)]"))
				cancel()
				return
			}
		}
	}()

	return func() {
		close(done)
		// Wait until the listener has stopped reading, so it cannot swallow a key meant
		// for the prompt or menu that follows the turn.
		<-exited
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, oldTermios)
	}
}

type SelectItem struct {
	ID       string
	Text     string
	IsActive bool
}

type SelectCallbacks struct {
	OnRename func(item SelectItem, newName string) (string, error)
	OnDelete func(item SelectItem) error
}

// FilterItems filters items matching a search query.
func FilterItems(items []SelectItem, query string) []SelectItem {
	return filterItems(items, query)
}

func filterItems(items []SelectItem, query string) []SelectItem {
	q := strings.TrimSpace(strings.ToLower(query))
	if q == "" {
		res := make([]SelectItem, len(items))
		copy(res, items)
		return res
	}
	var res []SelectItem
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Text), q) || (it.ID != "" && strings.Contains(strings.ToLower(it.ID), q)) {
			res = append(res, it)
		}
	}
	return res
}

// readConfirmRaw asks a yes/no question and answers it from the first key: y or Y confirms,
// any other key (Enter, n, Esc, an arrow, Ctrl+C, Ctrl+D) declines. Keys that arrive in the
// same burst, such as a double press or the rest of an arrow's escape sequence, are discarded
// so they can neither change the answer nor leak into the menu.
func readConfirmRaw(prompt string) bool {
	prompt = clipEnd(prompt, rowWidth()-len("  "+" [y/N]: ")-1)
	os.Stdout.WriteString(fmt.Sprintf("\r\033[2K  %s [y/N]: ", theme.Warn(prompt)))
	b := make([]byte, 1)
	n, err := os.Stdin.Read(b)
	confirmed := err == nil && n == 1 && (b[0] == 'y' || b[0] == 'Y')
	flushStdin()
	return confirmed
}

// readStringRaw reads a line of text: printable characters type, Backspace erases and Enter
// submits. Like readConfirmRaw, any other key (Esc, an arrow, Tab, Ctrl+C, Ctrl+D or another
// control key) cancels, and the rest of its burst, such as an arrow's escape sequence, is
// discarded so it cannot leak into the menu.
func readStringRaw(fd int, prompt string, initial string) (string, bool) {
	var val strings.Builder
	val.WriteString(initial)
	b := make([]byte, 1)
	for {
		cursor := " "
		if theme.Enabled() {
			cursor = theme.Mark(" ")
		}
		shown := clipStart(val.String(), rowWidth()-len("  ")-VisibleWidth(prompt)-1)
		os.Stdout.WriteString(fmt.Sprintf("\r\033[2K  %s%s%s", theme.Info(prompt), shown, cursor))
		n, err := os.Stdin.Read(b)
		if err != nil || n == 0 {
			return "", false
		}
		switch ch := b[0]; {
		case ch == '\r' || ch == '\n':
			flushStdin()
			return strings.TrimSpace(val.String()), true
		case ch == 0x7f || ch == 0x08: // Backspace
			s := val.String()
			if len(s) > 0 {
				val.Reset()
				val.WriteString(s[:len(s)-1])
			}
		case ch >= 32 && ch <= 126:
			val.WriteByte(ch)
		case ch >= 0x80: // Bytes of a non-ASCII character are not typed, but do not cancel
		default: // Esc, an arrow, Tab, Ctrl+C, Ctrl+D or another control key cancels
			flushStdin()
			return "", false
		}
	}
}

// InteractiveSelect displays a searchable, scrollable menu in terminal raw mode. In
// accessibility mode it prints a numbered list and reads a line instead (see accessibleSelect).
// Arrow keys (Up/Down) navigate, printable chars filter the list in real-time.
// Enter confirms, Esc / Ctrl+C cancels.
// When cb is provided, Ctrl+E renames and Ctrl+D / Delete deletes items.
// Keys typed before the menu opened are discarded.
// Returns (selectedItem, confirmed, error).
func InteractiveSelect(
	title string,
	items []SelectItem,
	defaultIdx int,
	reader *bufio.Reader,
	cb *SelectCallbacks,
) (SelectItem, bool, error) {
	return interactiveSelect(title, items, defaultIdx, reader, cb, true)
}

// interactiveSelect is InteractiveSelect. flush discards keys typed before the menu opened,
// such as stray keys pressed while the model was working. The slash menu passes false: the
// keys after its "/" are the rest of a command typed faster than the menu could draw.
func interactiveSelect(
	title string,
	items []SelectItem,
	defaultIdx int,
	reader *bufio.Reader,
	cb *SelectCallbacks,
	flush bool,
) (SelectItem, bool, error) {
	if len(items) == 0 {
		return SelectItem{}, false, nil
	}
	if theme.Accessibility() {
		return accessibleSelect(title, items, reader, cb)
	}

	fd := int(os.Stdin.Fd())
	if !isTerminal(uintptr(fd)) {
		return selectWithoutTerminal(items, reader)
	}

	oldTermios, termErr := unix.IoctlGetTermios(fd, unix.TCGETS)
	if termErr != nil {
		return SelectItem{}, false, fmt.Errorf("menu unavailable: %w", termErr)
	}

	if flush {
		flushStdin()
	}

	raw := *oldTermios
	raw.Lflag &^= (unix.ICANON | unix.ECHO)
	raw.Iflag &^= unix.IXON // Ctrl+S is ignored, as at the prompt, instead of freezing output
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	// Deliver Ctrl+C as a key (handled in-app) instead of raising SIGINT, which
	// would quit the agent. Other signal keys such as Ctrl+Z keep working.
	raw.Cc[unix.VINTR] = 0

	_ = unix.IoctlSetTermios(fd, unix.TCSETS, &raw)
	defer func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, oldTermios)
	}()

	os.Stdout.WriteString("\x1b[?25l")       // Hide cursor
	defer os.Stdout.WriteString("\x1b[?25h") // Show cursor

	m := newSelectMenu(title, fd, items, defaultIdx, cb)
	m.render(true)
	return m.run()
}

// Number of item rows visible in the InteractiveSelect menu at once.
const selectWindowSize = 7

// selectMenuLines is the menu height: title, search line, item window and footer.
const selectMenuLines = 1 + 1 + selectWindowSize + 1

// selectMenu holds the state of an InteractiveSelect menu while it is on screen.
type selectMenu struct {
	title       string
	fd          int
	items       []SelectItem
	filtered    []SelectItem
	query       string
	selected    int
	windowStart int
	flashMsg    string
	cb          *SelectCallbacks
}

// newSelectMenu builds the menu state, highlighting defaultIdx when it is in range.
func newSelectMenu(title string, fd int, items []SelectItem, defaultIdx int, cb *SelectCallbacks) *selectMenu {
	filtered := filterItems(items, "")
	selected := defaultIdx
	if selected < 0 || selected >= len(filtered) {
		selected = 0
	}
	return &selectMenu{title: title, fd: fd, items: items, filtered: filtered, selected: selected, cb: cb}
}

// run reads keys until the menu is confirmed or cancelled, clearing the menu before it returns.
func (m *selectMenu) run() (SelectItem, bool, error) {
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if err != nil || n == 0 {
			m.clear()
			return SelectItem{}, false, err
		}
		if item, confirmed, done := m.handleKey(b[0]); done {
			m.clear()
			return item, confirmed, nil
		}
	}
}

// handleKey applies one keypress. It returns done=true when the menu should close,
// along with the item and confirmed flag describing the outcome.
func (m *selectMenu) handleKey(ch byte) (SelectItem, bool, bool) {
	switch {
	case ch == 0x1b:
		return m.handleEscape()
	case ch == '\t': // Tab also moves down
		m.move(true)
	case ch == '\r' || ch == '\n': // Enter confirms the selection
		if len(m.filtered) == 0 {
			return SelectItem{}, false, false
		}
		return m.filtered[m.selected], true, true
	case ch == 0x03: // Ctrl+C cancels
		return SelectItem{}, false, true
	case ch == 0x04: // Ctrl+D deletes when callbacks are present
		return SelectItem{}, false, m.deleteHighlighted()
	case ch == 0x05: // Ctrl+E renames when callbacks are present
		m.renameHighlighted()
	case ch == 0x7f || ch == 0x08: // Backspace
		m.flashMsg = ""
		if len(m.query) > 0 {
			m.setQuery(m.query[:len(m.query)-1])
			m.render(false)
		}
	case ch == 0x15 || ch == 0x17: // Ctrl+U or Ctrl+W clears the search query
		m.flashMsg = ""
		m.setQuery("")
		m.render(false)
	case ch >= 32 && ch <= 126: // Printable ASCII extends the filter query
		m.flashMsg = ""
		m.setQuery(m.query + string(ch))
		m.render(false)
	}
	return SelectItem{}, false, false
}

// handleEscape reads the remainder of an escape sequence and applies arrow keys,
// the Delete key, or a standalone Esc press.
func (m *selectMenu) handleEscape() (SelectItem, bool, bool) {
	switch seq := readEscapeSeq(m.fd); seq {
	case "": // Standalone Esc cancels
		return SelectItem{}, false, true
	case "[A", "OA": // Arrow Up
		m.move(false)
	case "[B", "OB": // Arrow Down
		m.move(true)
	case "[3~": // Delete key
		return SelectItem{}, false, m.deleteHighlighted()
	}
	return SelectItem{}, false, false
}

// move shifts the highlight down or up, wrapping at either end, and redraws the menu.
func (m *selectMenu) move(down bool) {
	m.flashMsg = ""
	if len(m.filtered) == 0 {
		return
	}
	if down {
		if m.selected < len(m.filtered)-1 {
			m.selected++
		} else {
			m.selected = 0
		}
	} else {
		if m.selected > 0 {
			m.selected--
		} else {
			m.selected = len(m.filtered) - 1
		}
	}
	m.render(false)
}

// setQuery replaces the search query, refilters the items and resets the highlight to the top.
func (m *selectMenu) setQuery(query string) {
	m.query = query
	m.filtered = filterItems(m.items, query)
	m.selected = 0
	m.windowStart = 0
}

// deleteHighlighted deletes the highlighted item after confirmation and redraws the menu.
// It reports true when no items remain, in which case the caller should close the menu.
func (m *selectMenu) deleteHighlighted() bool {
	if m.cb == nil || m.cb.OnDelete == nil || len(m.filtered) == 0 {
		return false
	}
	target := m.filtered[m.selected]
	if !readConfirmRaw(fmt.Sprintf("delete conversation \"%s\"?", menuLine(target.Text))) {
		m.flashMsg = "delete cancelled"
	} else if err := m.cb.OnDelete(target); err != nil {
		m.flashMsg = menuLine(fmt.Sprintf("error deleting: %v", err))
	} else {
		m.flashMsg = "deleted conversation"
		m.removeItem(target.ID)
		if len(m.items) == 0 {
			return true
		}
	}
	m.render(false)
	return false
}

// removeItem drops the item with the given ID and refilters, keeping the highlight in range.
func (m *selectMenu) removeItem(id string) {
	var remaining []SelectItem
	for _, it := range m.items {
		if it.ID != id {
			remaining = append(remaining, it)
		}
	}
	m.items = remaining
	m.filtered = filterItems(m.items, m.query)
	if m.selected >= len(m.filtered) {
		m.selected = len(m.filtered) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
}

// renameHighlighted prompts for a new name for the highlighted item, applies it through
// the rename callback and redraws the menu. As with delete, a key other than text, Backspace
// or Enter cancels the prompt, and so does submitting a blank name.
func (m *selectMenu) renameHighlighted() {
	if m.cb == nil || m.cb.OnRename == nil || len(m.filtered) == 0 {
		return
	}
	target := m.filtered[m.selected]
	newName, ok := readStringRaw(m.fd, "rename to: ", "")
	if !ok || newName == "" {
		m.flashMsg = "rename cancelled"
	} else if updatedText, err := m.cb.OnRename(target, newName); err != nil {
		m.flashMsg = menuLine(fmt.Sprintf("error renaming: %v", err))
	} else {
		for i := range m.items {
			if m.items[i].ID == target.ID {
				if updatedText != "" {
					m.items[i].Text = updatedText
				} else {
					m.items[i].Text = newName
				}
			}
		}
		m.filtered = filterItems(m.items, m.query)
		m.flashMsg = fmt.Sprintf("renamed to \"%s\"", newName)
	}
	m.render(false)
}

// clear erases the menu from the screen.
func (m *selectMenu) clear() {
	os.Stdout.WriteString(fmt.Sprintf("\033[%dA\033[0J", selectMenuLines))
}

// render draws the menu. After the first draw it moves the cursor back over the previous
// frame so the menu is redrawn in place.
func (m *selectMenu) render(firstTime bool) {
	m.scrollToSelection()

	var sb strings.Builder
	if !firstTime {
		sb.WriteString(fmt.Sprintf("\033[%dA", selectMenuLines))
	}

	// Line 1: Title & help
	width := rowWidth()
	sb.WriteString(fmt.Sprintf("\r\033[2K%s\n", theme.Muted(clipEnd(m.title, width))))

	// Line 2: Search line
	cursor := " "
	if theme.Enabled() {
		cursor = theme.Mark(" ")
	}
	query := clipStart(m.query, width-len("  > ")-1)
	sb.WriteString(fmt.Sprintf("\r\033[2K  %s %s%s\n", theme.Prompt(">"), query, cursor))

	// Lines 3 to (3 + selectWindowSize - 1): Items window
	m.writeItemWindow(&sb, width)

	// Last line: Footer
	m.writeFooter(&sb, width)

	// The cursor rests on the line below the footer, where the delete confirmation and the
	// rename prompt are drawn. Erase it so an answered prompt does not stay on screen.
	sb.WriteString("\r\033[2K")

	os.Stdout.WriteString(sb.String())
}

// scrollToSelection moves the visible window so the highlighted item is on screen.
func (m *selectMenu) scrollToSelection() {
	if m.selected < m.windowStart {
		m.windowStart = m.selected
	}
	if m.selected >= m.windowStart+selectWindowSize {
		m.windowStart = m.selected - selectWindowSize + 1
	}
	if m.windowStart < 0 {
		m.windowStart = 0
	}
}

// writeItemWindow writes the rows of the visible item window, padding unused rows with blanks.
func (m *selectMenu) writeItemWindow(sb *strings.Builder, width int) {
	for row := range selectWindowSize {
		itemIdx := m.windowStart + row
		if itemIdx < len(m.filtered) {
			it := m.filtered[itemIdx]
			marker := " "
			if it.IsActive {
				marker = "*"
			}
			label := fmt.Sprintf("%s %2d. ", marker, itemIdx+1)
			text := clipEnd(menuLine(it.Text), width-len("   ")-len(label))
			if itemIdx == m.selected {
				selGlyph := "❯"
				if theme.Accessibility() {
					selGlyph = ">"
				}
				sb.WriteString(fmt.Sprintf("\r\033[2K %s %s\n", theme.Accent(selGlyph), theme.Bold(label+text)))
			} else {
				sb.WriteString(fmt.Sprintf("\r\033[2K   %s%s\n", label, text))
			}
		} else if row == 0 && len(m.filtered) == 0 {
			sb.WriteString(fmt.Sprintf("\r\033[2K    %s\n", theme.Muted("(no matching results)")))
		} else {
			sb.WriteString("\r\033[2K\n")
		}
	}
}

// menuLine is item text as one row of a menu shows it. Item text can come from outside
// the program (a conversation name edited into the session file or written by a model), so
// escape sequences and other control characters are removed, and a Tab or newline, which
// would break the row and throw off the in-place redraw, becomes a space.
func menuLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return ' '
		}
		return r
	}, theme.Sanitize(s))
}

// writeFooter writes the status line: a flash message, or the match count with scroll badges.
func (m *selectMenu) writeFooter(sb *strings.Builder, width int) {
	sb.WriteString("\r\033[2K")
	switch {
	case m.flashMsg != "":
		sb.WriteString(fmt.Sprintf("  %s", theme.OK(clipEnd(m.flashMsg, width-2-VisibleWidth(theme.OK(""))))))
	case len(m.filtered) == 0:
		sb.WriteString(fmt.Sprintf("  %s", theme.Muted("0 matches")))
	default:
		footerText := fmt.Sprintf("%d/%d matches", m.selected+1, len(m.filtered))
		upGlyph, downGlyph := "↑", "↓"
		if theme.Accessibility() {
			upGlyph, downGlyph = "^", "v"
		}
		var badges []string
		if m.windowStart > 0 {
			badges = append(badges, fmt.Sprintf("%s %d more", upGlyph, m.windowStart))
		}
		remainingBelow := len(m.filtered) - (m.windowStart + selectWindowSize)
		if remainingBelow > 0 {
			badges = append(badges, fmt.Sprintf("%s %d more", downGlyph, remainingBelow))
		}
		if len(badges) > 0 {
			footerText += fmt.Sprintf(" [%s]", strings.Join(badges, ", "))
		}
		sb.WriteString(fmt.Sprintf("  %s", theme.Muted(clipEnd(footerText, width-2))))
	}
	sb.WriteString("\n")
}

// selectWithoutTerminal picks an item when stdin is not a terminal. It reads one line from
// reader and matches it as a 1-based number, an ID or a case-insensitive text fragment.
// End of input, an empty line or text matching no item cancels the menu.
func selectWithoutTerminal(items []SelectItem, reader *bufio.Reader) (SelectItem, bool, error) {
	if reader != nil {
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			return SelectItem{}, false, nil
		}
		choice := strings.TrimSpace(line)
		if choice == "" {
			return SelectItem{}, false, nil
		}
		// The whole answer must be a number: Sscanf would also read "2fast/gpt" as item 2.
		if idx, err := strconv.Atoi(choice); err == nil && idx >= 1 && idx <= len(items) {
			return items[idx-1], true, nil
		}
		for _, it := range items {
			if strings.EqualFold(it.ID, choice) || strings.Contains(strings.ToLower(it.Text), strings.ToLower(choice)) {
				return it, true, nil
			}
		}
	}
	return SelectItem{}, false, nil
}
