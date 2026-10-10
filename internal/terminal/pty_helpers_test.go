package terminal

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ptyScreen is a pseudo terminal wired to os.Stdin and os.Stdout for the duration of a
// test. Everything the code under test writes can be inspected, and keys are typed with
// Type. Tests synchronise on screen output (waitFor) instead of sleeping.
type ptyScreen struct {
	master, slave *os.File
	mu            sync.Mutex
	buf           strings.Builder
}

func newPTYScreen(t *testing.T) *ptyScreen {
	t.Helper()
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("PTY not available in environment: %v", err)
	}
	p := &ptyScreen{master: master, slave: slave}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, slave
	done := make(chan struct{})
	go func() {
		defer close(done)
		chunk := make([]byte, 4096)
		for {
			n, err := master.Read(chunk)
			p.mu.Lock()
			p.buf.Write(chunk[:n])
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		slave.Close()
		master.Close()
		<-done
	})
	return p
}

// promptMarks removes the invisible OSC 133 prompt marks, which most tests do not check.
var promptMarks = strings.NewReplacer(markPromptStart, "", markSecondaryStart, "", markInputStart, "", markOutputStart, "")

// String returns everything written to the terminal so far, without OSC 133 prompt marks.
func (p *ptyScreen) String() string { return promptMarks.Replace(p.Raw()) }

// Raw returns everything written to the terminal so far, byte for byte.
func (p *ptyScreen) Raw() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// Len is the number of bytes written so far; use it as a mark for waitForAfter.
func (p *ptyScreen) Len() int { return len(p.String()) }

// Type sends keystrokes as if typed.
func (p *ptyScreen) Type(t *testing.T, s string) {
	t.Helper()
	if _, err := p.master.WriteString(s); err != nil {
		t.Fatalf("typing %q: %v", s, err)
	}
}

func (p *ptyScreen) waitFor(t *testing.T, substr string) { t.Helper(); p.waitForAfter(t, 0, substr) }

// waitForAfter blocks until substr appears in the output written after offset.
func (p *ptyScreen) waitForAfter(t *testing.T, offset int, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := p.String(); len(s) >= offset && strings.Contains(s[offset:], substr) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; screen: %q", substr, p.String()[min(offset, len(p.String())):])
}

// setRaw puts the slave in non-canonical, no-echo mode before any key is sent.
func (p *ptyScreen) setRaw(t *testing.T) {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	tio.Lflag &^= unix.ICANON | unix.ECHO
	if err := unix.IoctlSetTermios(int(p.slave.Fd()), unix.TCSETS, tio); err != nil {
		t.Fatal(err)
	}
}

func (p *ptyScreen) canonical(t *testing.T) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&unix.ICANON != 0
}
