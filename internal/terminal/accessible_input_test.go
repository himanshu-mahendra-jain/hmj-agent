package terminal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"hmj-agent/internal/terminal/theme"
)

// swapStdin points os.Stdin at f for the rest of the test. A pipe is not a terminal, so the
// cooked-mode termios calls are skipped.
func swapStdin(t *testing.T, f *os.File) {
	t.Helper()
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })
}

// pipeStdin makes stdin a pipe, so tests never touch a developer's real terminal.
func pipeStdin(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	swapStdin(t, r)
}

// captureStdout returns what fn writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	os.Stdout = old
	w.Close()
	return <-out
}

func setAccessible(t *testing.T) {
	t.Helper()
	theme.SetAccessibility(true)
	theme.SetEnabled(false)
	t.Cleanup(func() {
		theme.SetAccessibility(false)
		theme.SetEnabled(false)
	})
}

func TestAccessibleReadLines(t *testing.T) {
	setAccessible(t)
	pipeStdin(t)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"single line", "hello\n", "hello"},
		{"backslash continues", "first \\\nsecond\n", "first \nsecond"},
		{"two continuations", "a\\\n\\\nb\n", "a\n\nb"},
		{"control byte dropped", "text\x0e\n", "text"},
		{"slash alone on a pipe is shortcuts", "/\n", "/shortcuts"},
		{"partial line at EOF", "partial", "partial"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			got, action, err := ReadSeamlessInput(reader)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if action != ActionNone {
				t.Errorf("action = %v, want ActionNone", action)
			}
			if got != tt.want {
				t.Errorf("input %q = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestAccessibleReadEOF(t *testing.T) {
	setAccessible(t)
	pipeStdin(t)

	reader := bufio.NewReader(strings.NewReader("only\n"))
	if got, _, err := ReadSeamlessInput(reader); err != nil || got != "only" {
		t.Fatalf("first read = (%q, %v), want (only, nil)", got, err)
	}
	if _, _, err := ReadSeamlessInput(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("read at end = %v, want io.EOF", err)
	}
}

// TestAccessibleReadCookedOnTerminal drives the accessible reader through a real pseudo
// terminal left in raw mode, as the halt listener leaves it. Ctrl+D only ends the input in
// canonical mode, so the EOF result proves the reader switched to cooked mode, and the
// original settings must be restored afterwards.
func TestAccessibleReadCookedOnTerminal(t *testing.T) {
	setAccessible(t)
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("PTY not available in environment: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	swapStdin(t, slave)

	raw, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	raw.Lflag &^= unix.ICANON | unix.ECHO
	if err := unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, raw); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(slave)
	go func() {
		time.Sleep(10 * time.Millisecond)
		master.Write([]byte("one\\\r"))
		master.Write([]byte("two\r"))
	}()

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, _, err := ReadSeamlessInput(reader)
		done <- result{text, err}
	}()
	select {
	case res := <-done:
		if res.text != "one\ntwo" {
			t.Errorf("text = %q, want %q", res.text, "one\ntwo")
		}
		if res.err != nil {
			t.Errorf("err = %v, want nil", res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("accessible reader did not finish: terminal was not in cooked mode")
	}

	// The second read sees Ctrl+D as end of input.
	go func() {
		time.Sleep(10 * time.Millisecond)
		master.Write([]byte{0x04})
	}()
	go func() {
		_, _, err := ReadSeamlessInput(reader)
		done <- result{err: err}
	}()
	select {
	case res := <-done:
		if !errors.Is(res.err, io.EOF) {
			t.Errorf("Ctrl+D err = %v, want io.EOF", res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl+D did not end the accessible reader")
	}

	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lflag&unix.ICANON != 0 {
		t.Errorf("termios not restored: ICANON is back on, expected the raw settings to return")
	}
}

func TestAccessibleCommandMenuOnTerminal(t *testing.T) {
	setAccessible(t)
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("PTY not available in environment: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	swapStdin(t, slave)

	reader := bufio.NewReader(slave)
	go func() {
		time.Sleep(10 * time.Millisecond)
		master.Write([]byte("/\r"))
		time.Sleep(10 * time.Millisecond)
		master.Write([]byte("2\r"))
	}()
	var got string
	var err2 error
	out := captureStdout(t, func() {
		got, _, err2 = ReadSeamlessInput(reader)
	})
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if got != "/resume" {
		t.Errorf("slash menu item 2 = %q, want /resume", got)
	}
	if !strings.Contains(out, "  2. /resume") {
		t.Errorf("expected numbered command list, got %q", out)
	}
	if strings.ContainsAny(out, "\r") || strings.Contains(out, "\x1b[") {
		t.Errorf("accessible menu wrote cursor or erase codes: %q", out)
	}
}

func TestAccessibleSelectAnswers(t *testing.T) {
	setAccessible(t)
	pipeStdin(t)

	items := []SelectItem{
		{ID: "a", Text: "alpha"},
		{ID: "b", Text: "beta", IsActive: true},
		{ID: "c", Text: "gamma"},
	}
	tests := []struct {
		name      string
		input     string
		wantID    string
		wantOK    bool
		wantEmpty bool
	}{
		{"number selects", "2\n", "b", true, false},
		{"empty line cancels", "\n", "", false, true},
		{"q cancels", "q\n", "", false, true},
		{"filter then number", "amm\n1\n", "c", true, false},
		{"star resets filter", "amm\n*\n1\n", "a", true, false},
		{"out of range number filters", "9\nq\n", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			var item SelectItem
			var ok bool
			var err error
			captureStdout(t, func() {
				item, ok, err = InteractiveSelect("pick one", items, 0, reader, nil)
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tt.wantOK || item.ID != tt.wantID {
				t.Errorf("got (%q, %v), want (%q, %v)", item.ID, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestAccessibleSelectPrintsNumberedList(t *testing.T) {
	setAccessible(t)
	pipeStdin(t)

	reader := bufio.NewReader(strings.NewReader("q\n"))
	out := captureStdout(t, func() {
		InteractiveSelect("pick one", []SelectItem{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}}, 0, reader, nil)
	})
	for _, want := range []string{"  1. alpha", "  2. beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output %q", want, out)
		}
	}
	if !strings.Contains(out, "Commands:") {
		t.Errorf("expected a one-line command hint, got %q", out)
	}
}

// TestAccessibleSelectEOF checks that the end of input (Ctrl+D) cancels the menu without an
// error, so the command that opened it does not report a failure.
func TestAccessibleSelectEOF(t *testing.T) {
	setAccessible(t)
	pipeStdin(t)

	reader := bufio.NewReader(strings.NewReader(""))
	var (
		ok  bool
		err error
	)
	captureStdout(t, func() {
		_, ok, err = InteractiveSelect("pick", []SelectItem{{ID: "a", Text: "alpha"}}, 0, reader, nil)
	})
	if ok || err != nil {
		t.Fatalf("got (confirmed %v, err %v), want cancelled without an error", ok, err)
	}
}

// fakeCallbacks records rename and delete calls. Deleting or renaming an unknown ID fails.
type fakeCallbacks struct {
	renamed []string
	deleted []string
}

func (f *fakeCallbacks) callbacks() *SelectCallbacks {
	return &SelectCallbacks{
		OnRename: func(item SelectItem, newName string) (string, error) {
			f.renamed = append(f.renamed, item.ID+"="+newName)
			return "", nil
		},
		OnDelete: func(item SelectItem) error {
			if item.ID == "bad" {
				return fmt.Errorf("cannot delete %s", item.ID)
			}
			f.deleted = append(f.deleted, item.ID)
			return nil
		},
	}
}

func TestAccessibleSelectRenameAndDelete(t *testing.T) {
	setAccessible(t)
	pipeStdin(t)

	items := []SelectItem{
		{ID: "a", Text: "alpha"},
		{ID: "b", Text: "beta"},
		{ID: "c", Text: "gamma"},
	}

	t.Run("rename then cancel", func(t *testing.T) {
		fake := &fakeCallbacks{}
		reader := bufio.NewReader(strings.NewReader("r 2 Renamed beta\nq\n"))
		var out string
		var ok bool
		var err error
		out = captureStdout(t, func() {
			_, ok, err = InteractiveSelect("pick", items, 0, reader, fake.callbacks())
		})
		if err != nil || ok {
			t.Fatalf("got (ok=%v, err=%v), want cancel", ok, err)
		}
		if len(fake.renamed) != 1 || fake.renamed[0] != "b=Renamed beta" {
			t.Errorf("rename calls = %v, want [b=Renamed beta]", fake.renamed)
		}
		if !strings.Contains(out, "renamed to Renamed beta") {
			t.Errorf("expected rename confirmation, got %q", out)
		}
	})

	t.Run("delete with confirmation then select", func(t *testing.T) {
		fake := &fakeCallbacks{}
		reader := bufio.NewReader(strings.NewReader("d 3\ny\n1\n"))
		var item SelectItem
		var ok bool
		captureStdout(t, func() {
			item, ok, _ = InteractiveSelect("pick", items, 0, reader, fake.callbacks())
		})
		if len(fake.deleted) != 1 || fake.deleted[0] != "c" {
			t.Errorf("delete calls = %v, want [c]", fake.deleted)
		}
		if !ok || item.ID != "a" {
			t.Errorf("after delete, selected (%q, %v), want (a, true)", item.ID, ok)
		}
	})

	t.Run("delete declined", func(t *testing.T) {
		fake := &fakeCallbacks{}
		reader := bufio.NewReader(strings.NewReader("d 1\nn\nq\n"))
		captureStdout(t, func() {
			InteractiveSelect("pick", items, 0, reader, fake.callbacks())
		})
		if len(fake.deleted) != 0 {
			t.Errorf("declined delete still deleted %v", fake.deleted)
		}
	})

	t.Run("delete failure is reported", func(t *testing.T) {
		fake := &fakeCallbacks{}
		bad := []SelectItem{{ID: "bad", Text: "broken"}}
		reader := bufio.NewReader(strings.NewReader("d 1\ny\n"))
		var out string
		out = captureStdout(t, func() {
			InteractiveSelect("pick", bad, 0, reader, fake.callbacks())
		})
		if !strings.Contains(out, "delete failed") {
			t.Errorf("expected failure message, got %q", out)
		}
	})

	t.Run("deleting the last item closes the menu", func(t *testing.T) {
		fake := &fakeCallbacks{}
		reader := bufio.NewReader(strings.NewReader("d 1\ny\n"))
		var ok bool
		var err error
		captureStdout(t, func() {
			_, ok, err = InteractiveSelect("pick", []SelectItem{{ID: "only", Text: "only"}}, 0, reader, fake.callbacks())
		})
		if err != nil || ok {
			t.Errorf("got (ok=%v, err=%v), want cancelled menu", ok, err)
		}
	})

	t.Run("commands need callbacks", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader("r 1 x\nq\n"))
		var out string
		out = captureStdout(t, func() {
			InteractiveSelect("pick", items, 0, reader, nil)
		})
		if !strings.Contains(out, "(no matching results)") {
			t.Errorf("without callbacks 'r 1 x' should filter, got %q", out)
		}
	})
}

// The accessible "/" command list names only the slash commands: the Ctrl+ hints belong to the
// key-driven menu and do not work in accessibility mode.
func TestAccessibleSlashCommandsHaveNoCtrlHints(t *testing.T) {
	items := accessibleSlashCommands()
	if len(items) != len(DefaultSlashCommands()) {
		t.Fatalf("accessible list has %d items, want %d", len(items), len(DefaultSlashCommands()))
	}
	for _, it := range items {
		if strings.Contains(it.Text, "Ctrl+") {
			t.Errorf("accessible command %q still shows a Ctrl+ hint", it.Text)
		}
		if !strings.HasPrefix(it.Text, it.ID) {
			t.Errorf("accessible command text %q should start with its command %q", it.Text, it.ID)
		}
	}
}
