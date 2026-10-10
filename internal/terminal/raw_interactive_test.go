package terminal

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHasInputPendingAndFlushStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	// Initially empty: hasInputPending should return false within 10ms
	has, err := hasInputPending(r.Fd(), 10*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Fatal("expected no input pending on empty pipe")
	}

	// Write 1 byte: hasInputPending should return true
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	has, err = hasInputPending(r.Fd(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !has {
		t.Fatal("expected input pending after write")
	}
}

func TestReadEscapeSeq(t *testing.T) {
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	defer func() {
		os.Stdin = oldStdin
		r.Close()
		w.Close()
	}()

	// Write ANSI arrow up sequence: "[A"
	if _, err := w.Write([]byte("[A")); err != nil {
		t.Fatal(err)
	}
	seq := readEscapeSeq(int(r.Fd()))
	if seq != "[A" {
		t.Fatalf("expected '[A', got %q", seq)
	}
}

// TestReadConfirmRaw checks that the first key decides: only y or Y confirms, and the rest of
// the same burst (a second key, an arrow's escape sequence) is discarded.
func TestReadConfirmRaw(t *testing.T) {
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	defer func() {
		os.Stdin = oldStdin
		r.Close()
		w.Close()
	}()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"y", "y", true},
		{"Y", "Y", true},
		{"y then Enter", "y\r", true},
		{"y then arrow", "y\x1b[B", true},
		{"yy double press", "yy", true},
		{"n", "n", false},
		{"Enter", "\r", false},
		{"Esc", "\x1b", false},
		{"Down arrow", "\x1b[B", false},
		{"Up arrow", "\x1b[A", false},
		{"Ctrl+C", "\x03", false},
		{"Ctrl+D", "\x04", false},
		{"n then y", "ny", false},
	}
	for _, tc := range cases {
		if _, err := w.Write([]byte(tc.input)); err != nil {
			t.Fatal(err)
		}
		if got := readConfirmRaw("Delete item?"); got != tc.want {
			t.Errorf("%s: readConfirmRaw = %v, want %v", tc.name, got, tc.want)
		}
		// Nothing from the burst may be left over for the next reader.
		if pending, _ := hasInputPending(r.Fd(), 20*time.Millisecond); pending {
			t.Errorf("%s: input left over after the answer", tc.name)
			flushStdin()
		}
	}
}

// TestReadStringRaw checks that text keys edit the name and Enter submits it, while any other
// key cancels like it does in readConfirmRaw, discarding the rest of its burst.
func TestReadStringRaw(t *testing.T) {
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	defer func() {
		os.Stdin = oldStdin
		r.Close()
		w.Close()
	}()

	cases := []struct {
		name    string
		initial string
		input   string
		want    string
		ok      bool
	}{
		{"text then Enter", "", "updated-convo\r", "updated-convo", true},
		{"text then newline", "", "updated-convo\n", "updated-convo", true},
		{"initial text kept", "old", "-x\r", "old-x", true},
		{"Backspace erases", "", "abcd\x7f\x08e\r", "abe", true},
		{"Backspace on empty", "", "\x7fa\r", "a", true},
		{"surrounding spaces trimmed", "", "  name  \r", "name", true},
		{"non-ASCII ignored", "", "caf\xc3\xa9\r", "caf", true},
		{"Enter then more keys", "", "a\rbc", "a", true},
		{"Esc", "old", "abc\x1b", "", false},
		{"Up arrow", "", "abc\x1b[A", "", false},
		{"Down arrow", "", "abc\x1b[B", "", false},
		{"Delete key", "", "abc\x1b[3~", "", false},
		{"Ctrl+C", "", "abc\x03", "", false},
		{"Ctrl+D", "", "abc\x04", "", false},
		{"Ctrl+E", "", "abc\x05", "", false},
		{"Tab", "", "abc\t", "", false},
		{"Ctrl+D then text", "", "\x04abc\r", "", false},
	}
	for _, tc := range cases {
		if _, err := w.Write([]byte(tc.input)); err != nil {
			t.Fatal(err)
		}
		val, ok := readStringRaw(int(r.Fd()), "Rename:", tc.initial)
		if val != tc.want || ok != tc.ok {
			t.Errorf("%s: readStringRaw = (%q, %v), want (%q, %v)", tc.name, val, ok, tc.want, tc.ok)
		}
		// Nothing from the burst may be left over for the next reader.
		if pending, _ := hasInputPending(r.Fd(), 20*time.Millisecond); pending {
			t.Errorf("%s: input left over after the answer", tc.name)
			flushStdin()
		}
	}
}

func TestHexVal(t *testing.T) {
	if hexVal('0') != 0 || hexVal('9') != 9 || hexVal('a') != 10 || hexVal('f') != 15 || hexVal('A') != 10 || hexVal('F') != 15 {
		t.Fatal("hexVal returned unexpected results")
	}
	if hexVal('z') != -1 {
		t.Fatal("expected -1 for non-hex character")
	}
}

func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	ptn, err := unix.IoctlGetUint32(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptn), os.O_RDWR, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func TestReadSeamlessInputPTY(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("PTY not available in environment: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	oldStdin := os.Stdin
	os.Stdin = slave
	defer func() {
		os.Stdin = oldStdin
	}()

	reader := bufio.NewReader(slave)

	// 1. Normal line typing + Enter
	go func() {
		time.Sleep(10 * time.Millisecond)
		if _, err := master.Write([]byte("hello world\r")); err != nil {
			t.Errorf("failed to write to PTY master: %v", err)
		}
	}()
	input, action, err := ReadSeamlessInput(reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != ActionNone || input != "hello world" {
		t.Fatalf("expected ('hello world', ActionNone), got (%q, %v)", input, action)
	}

	// 2. Bracketed paste mode: \x1b[200~line1\nline2\x1b[201~\r
	go func() {
		time.Sleep(10 * time.Millisecond)
		if _, err := master.Write([]byte("\x1b[200~first line\nsecond line\x1b[201~\r")); err != nil {
			t.Errorf("failed to write paste sequence to PTY master: %v", err)
		}
	}()
	input, action, err = ReadSeamlessInput(reader)
	if err != nil {
		t.Fatalf("unexpected error on paste: %v", err)
	}
	if !strings.Contains(input, "first line") || !strings.Contains(input, "second line") {
		t.Fatalf("expected multi-line paste content, got %q", input)
	}

	// 3. Shortcut keys: Ctrl+N (0x0e), Ctrl+R (0x12), Ctrl+O (0x0f), Ctrl+Y (0x19)
	cases := []struct {
		key    byte
		action ShortcutAction
	}{
		{0x0e, ActionNew},
		{0x12, ActionResume},
		{0x0f, ActionModels},
		{0x19, ActionHistory},
	}
	for _, c := range cases {
		go func(k byte) {
			time.Sleep(10 * time.Millisecond)
			if _, err := master.Write([]byte{k}); err != nil {
				t.Errorf("failed to write key 0x%02x to PTY master: %v", k, err)
			}
		}(c.key)
		_, act, err := ReadSeamlessInput(reader)
		if err != nil {
			t.Fatalf("unexpected error on shortcut key 0x%02x: %v", c.key, err)
		}
		if act != c.action {
			t.Fatalf("for key 0x%02x: expected action %v, got %v", c.key, c.action, act)
		}
	}

	// 4. Unbound control keys (Ctrl+L, Ctrl+B) are ignored rather than typed into the
	// prompt, while Tab is still inserted.
	go func() {
		time.Sleep(10 * time.Millisecond)
		if _, err := master.Write([]byte("\x0ca\x02b\tc\r")); err != nil {
			t.Errorf("failed to write control keys to PTY master: %v", err)
		}
	}()
	input, action, err = ReadSeamlessInput(reader)
	if err != nil {
		t.Fatalf("unexpected error after unbound control keys: %v", err)
	}
	if action != ActionNone || input != "ab\tc" {
		t.Fatalf("expected (%q, ActionNone) after unbound control keys, got (%q, %v)", "ab\tc", input, action)
	}
}
