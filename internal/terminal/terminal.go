package terminal

import (
	"fmt"
	"os"
	"sync"
	"time"

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
	setTermios(fd, tio)
}

// termiosWarning reports the first failed change of terminal mode; later ones are the same
// failure and would only repeat it.
var termiosWarning sync.Once

// setTermios applies t to the terminal on fd. A failure leaves the terminal as it was, which
// may echo keys or wait for Enter; there is nothing better to do than say so, once, on stderr.
func setTermios(fd int, t *unix.Termios) {
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		warnTermios(err)
	}
}

func warnTermios(err error) {
	termiosWarning.Do(func() { fmt.Fprintf(os.Stderr, "warning: could not set the terminal mode: %v\n", err) })
}

func Restore() {
	termiosMu.Lock()
	defer termiosMu.Unlock()
	DisableBracketedPaste()
	if isTerminal(os.Stdout.Fd()) {
		os.Stdout.WriteString("\x1b[?25h")
	}
	if origState != nil {
		if err := term.Restore(int(os.Stdin.Fd()), origState); err != nil {
			warnTermios(err)
		}
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
