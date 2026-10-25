package terminal

import (
	"context"
	"fmt"
	"hmj-agent/v2/internal/terminal/theme"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

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

// StartHaltListener listens for the Escape key (or Ctrl+C) while the agent is running. On one,
// it stops the spinner, says so and calls cancel.
func StartHaltListener(cancel context.CancelFunc) func() {
	return ListenForHalt(func(key string) {
		StopActiveSpinner()
		fmt.Printf("\n%s\n", HaltNotice(key))
		cancel()
	})
}

// TurnDivider is the dim rule that sets a finished turn off from the next prompt. It is at most
// 80 columns wide, so a window narrowed later rewraps it only when it gets that narrow, and plain
// dashes in accessibility mode.
func TurnDivider() string {
	glyph := "─"
	if theme.Accessibility() {
		glyph = "-"
	}
	return theme.Muted(strings.Repeat(glyph, min(rowWidth(), 80)))
}

// HaltNotice is the transcript's line saying the user halted the work with key.
func HaltNotice(key string) string {
	return theme.Warn("[halted by user (" + key + ")]")
}

// ListenForHalt listens for the Escape key (or Ctrl+C) and calls onHalt once, with "Esc" or
// "Ctrl+C", from its own goroutine, printing nothing. The function it returns stops it.
func ListenForHalt(onHalt func(key string)) func() {
	return ListenForKeys(onHalt, nil)
}

// ListenForKeys is ListenForHalt that also calls onBackground, when set, each time Ctrl+B is
// pressed, and keeps listening.
func ListenForKeys(onHalt func(key string), onBackground func()) func() {
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

	setTermios(fd, &raw)

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
				onHalt("Ctrl+C")
				return
			}

			if b[0] == 0x02 && onBackground != nil {
				onBackground()
				continue
			}

			if b[0] == 0x1b {
				seq := readEscapeSeq(fd)
				if seq != "" {
					continue
				}

				onHalt("Esc")
				return
			}
		}
	}()

	return func() {
		close(done)
		// Wait until the listener has stopped reading, so it cannot swallow a key meant
		// for the prompt or menu that follows the turn.
		<-exited
		setTermios(fd, oldTermios)
	}
}
