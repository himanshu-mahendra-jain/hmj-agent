package terminal

import (
	"bufio"
	"errors"
	"fmt"
	"hmj-agent/internal/terminal/theme"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type ShortcutAction int

const (
	ActionNone ShortcutAction = iota
	ActionNew
	ActionResume
	ActionModels
	ActionHistory
	// ActionWake ends an empty prompt when the wake channel given to ReadSeamlessInput
	// fires, such as when a background job has ended.
	ActionWake
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
// It also ends with ActionWake when wake is closed while nothing has been typed. Once it
// fires over typed text it is ignored, so the text is never taken from under the user.
// Accessibility mode and piped input do not wake; a nil wake never fires.
func ReadSeamlessInput(reader *bufio.Reader, wake <-chan struct{}) (string, ShortcutAction, error) {
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

	setTermios(fd, &raw)
	EnableBracketedPaste()

	defer func() {
		DisableBracketedPaste()
		setTermios(fd, oldTermios)
	}()

	s := &seamlessInput{fd: fd, reader: reader, ed: NewPromptEditor(initial), wake: wake}
	if wake != nil {
		pipe, err := newWakePipe(wake)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %v\n", theme.Muted("[a background job's end will not start a turn:]"), err)
			s.wake = nil
		} else {
			defer func() {
				if err := pipe.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "%s %v\n", theme.Muted("[closing the wake pipe:]"), err)
				}
			}()
			s.wakeFd = pipe.r
		}
	}
	return s.run()
}

// wakePipe makes a channel closing readable as a file descriptor, so one poll can wait for
// it and for a key together instead of waking up to check the channel. The write end is
// closed when the channel closes, which the read end reports as a hang-up.
type wakePipe struct {
	r, w   int
	done   chan struct{}
	exited chan struct{}
	err    error // closing w, set before exited closes
}

func newWakePipe(wake <-chan struct{}) (*wakePipe, error) {
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_CLOEXEC); err != nil {
		return nil, fmt.Errorf("creating the wake pipe: %w", err)
	}
	p := &wakePipe{r: fds[0], w: fds[1], done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(p.exited)
		select {
		case <-wake:
		case <-p.done:
		}
		p.err = unix.Close(p.w)
	}()
	return p, nil
}

// Close stops the goroutine, which closes the write end, then closes the read end.
func (p *wakePipe) Close() error {
	close(p.done)
	<-p.exited
	return errors.Join(p.err, unix.Close(p.r))
}

// seamlessInput is the state of one ReadSeamlessInput call while the terminal is in raw mode.
type seamlessInput struct {
	fd       int
	reader   *bufio.Reader
	ed       *PromptEditor
	inPaste  bool
	pasteBuf strings.Builder
	pending  []byte // bytes decodeRune read past a cut-short character, handled next
	wake     <-chan struct{}
	wakeFd   int // readable once wake is closed; polled with the terminal
}

// run reads keys until the input ends, then returns the text entered so far.
func (s *seamlessInput) run() (string, ShortcutAction, error) {
	b := make([]byte, 1)
	for {
		if len(s.pending) == 0 && s.awoken() {
			_, text, action, err := s.endWithAction(ActionWake)
			return text, action, err
		}
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

// awoken waits for a key or for the wake channel, and reports whether the input should end
// for the wake: it fired while nothing is typed. It returns false at once without a channel.
func (s *seamlessInput) awoken() bool {
	if s.wake == nil {
		return false
	}
	fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}, {Fd: int32(s.wakeFd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || fds[0].Revents != 0 {
			return false // a key, or a read that reports what is wrong
		}
		break
	}
	if len(s.ed.runes) == 0 && !s.inPaste {
		return true
	}
	s.wake = nil // the user is typing: what woke goes with their message
	return false
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

func CleanInput(s string) string {
	s = strings.ReplaceAll(s, "\x1b[200~", "")
	s = strings.ReplaceAll(s, "\x1b[201~", "")
	s = strings.ReplaceAll(s, "^[[200~", "")
	s = strings.ReplaceAll(s, "^[[201~", "")
	return strings.TrimSpace(s)
}
