package terminal

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"hmj-agent/internal/terminal/theme"
)

// FuzzSeamlessInputKeys types arbitrary bytes, escape sequences and bracketed pastes
// included, into the raw-mode prompt and closes the terminal. Whatever the keys, reading
// must end without panicking, and the text it returns must be clean: no escape sequence or
// other control character beyond newlines and Tabs, and valid UTF-8.
func FuzzSeamlessInputKeys(f *testing.F) {
	for _, seed := range []string{
		"hello\r",
		"abc\x1b[D\x1b[DX\r",
		"\x1b[200~pasted\r\nlines\x1b[31m red\x1b[0m\x1b[201~\r",
		"\x1b[200~\x1b]0;title\x07\x08\x07\x1b[201~",
		"\x1b[200~unterminated paste",
		"日本語\x1b[D\x7f🚀e\u0301\r",
		"\x1bOA\x1bOB\x1b[1;5D\x1b[3;5~\x1bb\x1bf\x1bd\x1b\x7f",
		"word one two\x17\x15\x0b\x01\x05\r",
		"\xc2\x9b2J\xff\xfe\xe6\x97\r",
		"\x1b[",
		"\x03\x03x\x04",
		"\x0e",
		"/zz\x1b",
		"/",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, keys []byte) {
		master, slave := openPollablePTY(t)
		go io.Copy(io.Discard, master) // drain what the prompt draws
		oldIn, oldOut := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = slave, slave
		defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

		type result struct {
			text string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			text, _, err := ReadSeamlessInput(bufio.NewReader(slave), nil)
			done <- result{text, err}
		}()
		waitUntilRaw(t, slave) // keys typed before raw mode would be line-edited by the terminal
		if _, err := master.Write(keys); err != nil {
			t.Fatal(err)
		}
		// Once every key has been read, hang up: the prompt sees the end of its input. A key
		// such as Enter or a shortcut may end the prompt before the rest is read.
		var r result
		deadline := time.Now().Add(10 * time.Second)
	wait:
		for {
			select {
			case r = <-done:
				break wait
			default:
			}
			n, err := unix.IoctlGetInt(int(slave.Fd()), unix.TIOCINQ)
			if err != nil || n == 0 {
				master.Close()
				select {
				case r = <-done:
				case <-time.After(10 * time.Second):
					t.Fatalf("the prompt did not return after its input ended; keys %q", keys)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the prompt stopped reading keys with %d bytes left of %q", n, keys)
			}
			time.Sleep(time.Millisecond)
		}
		master.Close()
		slave.Close()
		if !utf8.ValidString(r.text) {
			t.Fatalf("invalid UTF-8 returned for %q: %q", keys, r.text)
		}
		for _, c := range r.text {
			if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f || (c >= 0x80 && c <= 0x9f) {
				t.Fatalf("control character %U returned for %q: %q", c, keys, r.text)
			}
		}
	})
}

// openPollablePTY opens a pseudo terminal whose two ends are in non-blocking mode, so
// closing one interrupts a read in progress on it, as a hang-up does.
func openPollablePTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, s, err := openPTY()
	if err != nil {
		t.Fatalf("opening a PTY: %v", err)
	}
	pollable := func(f *os.File) *os.File {
		fd, err := unix.Dup(int(f.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
		return os.NewFile(uintptr(fd), f.Name())
	}
	return pollable(m), pollable(s)
}

// waitUntilRaw waits until the terminal leaves canonical mode.
func waitUntilRaw(t *testing.T, tty *os.File) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tio, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if tio.Lflag&unix.ICANON == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the prompt never switched the terminal to raw mode")
		}
		time.Sleep(time.Millisecond)
	}
}

// FuzzMarkdownTableRendering streams arbitrary markdown, sanitized as model text is and split
// at an arbitrary point, through the table streamer. It must not panic and every line that
// is not a table row must come out unchanged. Every table found in the text must render as
// a grid whose rows all have the same width.
func FuzzMarkdownTableRendering(f *testing.F) {
	f.Add("| a | b |\n|---|:-:|\n| 1 | 2 |\n", 5)
	f.Add("text\n| h1 | h2 | h3 |\n| --- | ---: | :--- |\n| **bold** | `code` | *it* |\n| x |\nafter", 17)
	f.Add("| 名前 | 🚀 |\n|---|---|\n| cafe\u0301 | \u2764\ufe0f |\n", 3)
	f.Add("| long |\n|---|\n| "+strings.Repeat("word ", 60)+"|\n| "+strings.Repeat("x", 300)+" |\n", 40)
	f.Add("| a |\n|---|\n| x<br>y<br/>z |", 0)
	f.Add("| not a table |\n| still not |\n", 1)
	f.Add("||\n|-|\n", 2)
	f.Add("| a\tb |\n|-|\n| \x1b[31mred |\n", 1)
	f.Add("| **Name** | `*.go` |\n|---|---|\n| 5 * 3 | **unclosed |\n| **bold words that wrap around the column edge** | x |\n", 9)
	f.Fuzz(func(t *testing.T, md string, split int) {
		md = theme.Sanitize(md)
		var out strings.Builder
		ts := NewMarkdownStreamer(func(s string) { out.WriteString(s) })
		split = min(max(split, 0), len(md))
		ts.Write(md[:split])
		ts.Write(md[split:])
		ts.Flush()
		rendered := out.String()

		lines := strings.Split(md, "\n")
		for i, line := range lines {
			if line != "" && !isPotentialTableRow(line) && !strings.Contains(rendered, line) {
				t.Fatalf("prose line %q lost from the output %q", line, rendered)
			}
			tbl, ok := ParseMarkdownTable(lines[i:])
			if !ok {
				continue
			}
			for _, colour := range []bool{false, true} { // ASCII grid, and styled box drawing
				theme.SetEnabled(colour)
				rows := strings.Split(strings.TrimSuffix(tbl.Render(), "\n"), "\n")
				theme.SetEnabled(false)
				w := VisibleWidth(rows[0])
				for _, row := range rows {
					if VisibleWidth(row) != w || strings.ContainsRune(row, '\t') {
						t.Fatalf("table rows of different widths (colour %v) for %q:\n%s", colour, lines[i:], strings.Join(rows, "\n"))
					}
				}
			}
		}
	})
}
