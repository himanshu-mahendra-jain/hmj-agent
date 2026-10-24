package terminal

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInteractiveSelectOnTerminal(t *testing.T) {
	items := numberedItems(5)
	items[3].IsActive = true

	run := func(t *testing.T, screen *ptyScreen, cb *SelectCallbacks, def int, drive func()) (SelectItem, bool, string, error) {
		t.Helper()
		type res struct {
			item SelectItem
			ok   bool
			err  error
		}
		ch := make(chan res, 1)
		go func() {
			it, ok, err := InteractiveSelect("pick", append([]SelectItem(nil), items...), def, nil, cb)
			ch <- res{it, ok, err}
		}()
		screen.waitFor(t, "pick")
		drive()
		r := <-ch
		return r.item, r.ok, screen.String(), r.err
	}

	t.Run("arrows then Enter", func(t *testing.T) {
		screen := newPTYScreen(t)
		item, ok, out, err := run(t, screen, nil, 0, func() { screen.Type(t, "\x1b[B\x1b[B\r") })
		if err != nil || !ok || item.ID != "id3" {
			t.Fatalf("got (%v, %v, %v), want id3 confirmed", item, ok, err)
		}
		screen.waitFor(t, "\x1b[?25h")
		if !strings.HasPrefix(out, "\x1b[?25l") {
			t.Errorf("cursor not hidden when the menu opened: %q", out)
		}
		if !screen.canonical(t) {
			t.Error("terminal mode not restored after the menu closed")
		}
	})
	t.Run("default index and filter", func(t *testing.T) {
		screen := newPTYScreen(t)
		item, ok, _, _ := run(t, screen, nil, 2, func() { screen.Type(t, "\r") })
		if !ok || item.ID != "id3" {
			t.Fatalf("default highlight: got %v %v", item, ok)
		}
		screen2 := newPTYScreen(t)
		item, ok, _, _ = run(t, screen2, nil, 0, func() { screen2.Type(t, "item 5\r") })
		if !ok || item.ID != "id5" {
			t.Fatalf("filter: got %v %v", item, ok)
		}
	})
	t.Run("Esc cancels", func(t *testing.T) {
		screen := newPTYScreen(t)
		item, ok, _, err := run(t, screen, nil, 0, func() { screen.Type(t, "\x1b") })
		if ok || err != nil || item.ID != "" {
			t.Fatalf("got (%v, %v, %v), want cancelled", item, ok, err)
		}
	})
	t.Run("rename then delete through callbacks", func(t *testing.T) {
		screen := newPTYScreen(t)
		var renamed, deleted []string
		cb := &SelectCallbacks{
			OnRename: func(it SelectItem, n string) (string, error) { renamed = append(renamed, it.ID+"="+n); return n, nil },
			OnDelete: func(it SelectItem) error { deleted = append(deleted, it.ID); return nil },
		}
		_, ok, _, _ := run(t, screen, cb, 0, func() {
			screen.Type(t, "\x05")
			screen.waitFor(t, "rename to:")
			screen.Type(t, "fresh\r")
			screen.waitFor(t, `renamed to "fresh"`)
			mark := screen.Len()
			screen.Type(t, "\x04")
			screen.waitForAfter(t, mark, "[y/N]")
			screen.Type(t, "y")
			screen.waitForAfter(t, mark, "deleted conversation")
			screen.Type(t, "\r")
		})
		if !ok {
			t.Fatal("expected the second item to be confirmed")
		}
		if len(renamed) != 1 || renamed[0] != "id1=fresh" || len(deleted) != 1 || deleted[0] != "id1" {
			t.Errorf("renamed=%v deleted=%v", renamed, deleted)
		}
	})
}

func TestInteractiveSelectNonTerminalAndEmpty(t *testing.T) {
	if item, ok, err := InteractiveSelect("t", nil, 0, nil, nil); ok || err != nil || item.ID != "" {
		t.Errorf("empty list = (%v, %v, %v)", item, ok, err)
	}
	pipeStdin(t)
	items := numberedItems(3)
	tests := []struct {
		input  string
		wantID string
	}{{"2\n", "id2"}, {"ID3\n", "id3"}, {"item 1\n", "id1"}, {"\n", ""}, {"nomatch\n", ""}, {"9\n", ""}}
	for _, tc := range tests {
		item, ok, err := InteractiveSelect("t", items, 0, bufio.NewReader(strings.NewReader(tc.input)), nil)
		if err != nil || ok != (tc.wantID != "") || item.ID != tc.wantID {
			t.Errorf("input %q = (%v, %v, %v), want id %q", tc.input, item, ok, err, tc.wantID)
		}
	}
}

func readSeamless(reader *bufio.Reader) <-chan struct {
	text   string
	action ShortcutAction
	err    error
} {
	ch := make(chan struct {
		text   string
		action ShortcutAction
		err    error
	}, 1)
	go func() {
		text, action, err := ReadSeamlessInput(reader, nil)
		ch <- struct {
			text   string
			action ShortcutAction
			err    error
		}{text, action, err}
	}()
	return ch
}

func TestSlashMenuInPrompt(t *testing.T) {
	t.Run("confirmed command ends the input", func(t *testing.T) {
		screen := newPTYScreen(t)
		ch := readSeamless(bufio.NewReader(strings.NewReader("")))
		screen.Type(t, "/")
		screen.waitFor(t, "commands (type to filter")
		screen.Type(t, "res\r")
		r := <-ch
		if r.err != nil || r.text != "/resume" || r.action != ActionNone {
			t.Fatalf("got %+v, want /resume", r)
		}
		screen.waitFor(t, "> /resume\r")
	})
	t.Run("command typed in one burst is not dropped", func(t *testing.T) {
		// A paste, fast typing or SSH batching delivers the keys after "/" before the
		// menu has drawn; they must reach the menu rather than be flushed.
		screen := newPTYScreen(t)
		ch := readSeamless(bufio.NewReader(strings.NewReader("")))
		screen.Type(t, "/health\r")
		select {
		case r := <-ch:
			if r.err != nil || r.text != "/health" || r.action != ActionNone {
				t.Fatalf("got %+v, want /health", r)
			}
		case <-time.After(5 * time.Second):
			// Close the empty menu and end the prompt so the reader goroutine returns.
			screen.Type(t, "\x1b")
			time.Sleep(100 * time.Millisecond)
			screen.Type(t, "\x04")
			<-ch
			t.Fatalf("menu swallowed the typed-ahead command; screen: %q", screen.String())
		}
	})
	t.Run("cancel clears the prompt and keeps reading", func(t *testing.T) {
		screen := newPTYScreen(t)
		ch := readSeamless(bufio.NewReader(strings.NewReader("")))
		screen.Type(t, "/")
		screen.waitFor(t, "commands (type to filter")
		mark := screen.Len()
		screen.Type(t, "\x1b")
		screen.waitForAfter(t, mark, "\r\x1b[J> ")
		screen.Type(t, "hi\r")
		r := <-ch
		if r.err != nil || r.text != "hi" {
			t.Fatalf("got %+v, want text hi (the '/' must not be kept)", r)
		}
	})
	t.Run("slash after text is a literal character", func(t *testing.T) {
		screen := newPTYScreen(t)
		ch := readSeamless(bufio.NewReader(strings.NewReader("")))
		screen.Type(t, "a/b\r")
		if r := <-ch; r.text != "a/b" {
			t.Fatalf("got %q, want a/b", r.text)
		}
		if strings.Contains(screen.String(), "commands (type") {
			t.Error("menu opened for a mid-text slash")
		}
	})
}

func TestReadSeamlessInputEditingKeys(t *testing.T) {
	screen := newPTYScreen(t)
	ch := readSeamless(bufio.NewReader(strings.NewReader("")))
	// abc, Left, X -> abXc; Delete removes c; Home Y; End Z; Right is a no-op at the end;
	// the UTF-8 characters é and 日 arrive as multi-byte sequences.
	screen.Type(t, "abc\x1b[DX\x1b[3~\x1b[HY\x1b[F\x1b[4~Zé日\r")
	r := <-ch
	if r.err != nil || r.text != "YabXZé日" {
		t.Fatalf("got %+v, want YabXZé日", r)
	}
}

func TestReadSeamlessInputStartsWithBufferedText(t *testing.T) {
	screen := newPTYScreen(t)
	reader := bufio.NewReader(strings.NewReader("draft"))
	if _, err := reader.Peek(1); err != nil {
		t.Fatal(err)
	}
	ch := readSeamless(reader)
	screen.waitFor(t, "> draft")
	screen.Type(t, "!\r")
	if r := <-ch; r.text != "draft!" {
		t.Fatalf("got %q, want draft!", r.text)
	}
	if reader.Buffered() != 0 {
		t.Error("buffered bytes should have been consumed")
	}
}

func TestDrainBuffered(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("pasted text"))
	if got := drainBuffered(r); got != "" {
		t.Errorf("nothing buffered yet, got %q", got)
	}
	r.Peek(1)
	if got := drainBuffered(r); got != "pasted text" {
		t.Errorf("drainBuffered = %q, want the whole buffer", got)
	}
	if got := drainBuffered(r); got != "" {
		t.Errorf("second drain = %q, want empty", got)
	}
}

func TestSeamlessDecodeRune(t *testing.T) {
	tests := []struct {
		name  string
		first byte
		rest  string
		want  rune
	}{
		{"ascii needs no more bytes", 'a', "zzz", 'a'},
		{"two byte", 0xc3, "\xa9", 'é'},
		{"three byte", 0xe6, "\x97\xa5", '日'},
		{"four byte", 0xf0, "\x9f\x98\x80", '😀'},
		{"truncated at end of input", 0xe6, "\x97", '�'},
		{"lone continuation byte", 0x80, "", '�'},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newMenuEnv(t)
			env.feed(tc.rest)
			env.closeInput()
			s := &seamlessInput{}
			if got := s.decodeRune(tc.first); got != tc.want {
				t.Errorf("decodeRune = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("consumes only the needed continuation bytes", func(t *testing.T) {
		env := newMenuEnv(t)
		env.feed("\xa9Z")
		env.closeInput()
		(&seamlessInput{}).decodeRune(0xc3)
		b := make([]byte, 1)
		if _, err := os.Stdin.Read(b); err != nil || b[0] != 'Z' {
			t.Errorf("next byte = %q (%v), want Z", b, err)
		}
	})
}

func TestSeamlessHandleEscapeSequences(t *testing.T) {
	newInput := func(text string, cursor int) (*seamlessInput, *menuEnv) {
		env := newMenuEnv(t)
		ed := NewPromptEditor(text)
		ed.cursor = cursor
		return &seamlessInput{fd: env.fdIn(), ed: ed}, env
	}
	tests := []struct {
		name       string
		seq        string
		text       string
		cursor     int
		wantText   string
		wantCursor int
	}{
		{"left", "[D", "abc", 2, "abc", 1},
		{"right", "[C", "abc", 1, "abc", 2},
		{"up to previous line keeps column", "[A", "abcd\nxy", 7, "abcd\nxy", 2},
		{"down clamps to shorter line", "[B", "abcd\nxy", 3, "abcd\nxy", 7},
		{"home", "[H", "ab\ncd", 4, "ab\ncd", 3},
		{"home alt form", "[1~", "ab\ncd", 5, "ab\ncd", 3},
		{"end", "[F", "ab\ncd", 3, "ab\ncd", 5},
		{"end alt form", "[4~", "ab\ncd", 0, "ab\ncd", 2},
		{"delete", "[3~", "abc", 1, "ac", 1},
		{"delete at end is a no-op", "[3~", "abc", 3, "abc", 3},
		{"standalone Esc wipes the text", "", "abc", 3, "", 0},
		{"unknown sequence is ignored", "[Z", "abc", 1, "abc", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, env := newInput(tc.text, tc.cursor)
			env.feed(tc.seq)
			env.closeInput()
			s.handleEscape()
			if s.ed.Text() != tc.wantText || s.ed.Cursor() != tc.wantCursor {
				t.Errorf("text=%q cursor=%d, want %q %d", s.ed.Text(), s.ed.Cursor(), tc.wantText, tc.wantCursor)
			}
		})
	}
}

// Terminals in application-cursor mode send arrows and Home/End as SS3
// sequences (ESC O <final>) instead of CSI (ESC [ <final>).
func TestSeamlessSS3Sequences(t *testing.T) {
	tests := []struct {
		name       string
		seq        string
		text       string
		cursor     int
		wantCursor int
	}{
		{"left", "OD", "abc", 2, 1},
		{"right", "OC", "abc", 1, 2},
		{"up", "OA", "abcd\nxy", 7, 2},
		{"down", "OB", "abcd\nxy", 3, 7},
		{"home", "OH", "ab\ncd", 4, 3},
		{"end", "OF", "ab\ncd", 3, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newMenuEnv(t)
			ed := NewPromptEditor(tc.text)
			ed.cursor = tc.cursor
			s := &seamlessInput{fd: env.fdIn(), ed: ed}
			env.feed(tc.seq)
			env.closeInput()
			s.handleEscape()
			if s.ed.Text() != tc.text || s.ed.Cursor() != tc.wantCursor {
				t.Errorf("text=%q cursor=%d, want %q %d", s.ed.Text(), s.ed.Cursor(), tc.text, tc.wantCursor)
			}
		})
	}
}

func TestSeamlessBracketedPaste(t *testing.T) {
	env := newMenuEnv(t)
	s := &seamlessInput{fd: env.fdIn(), ed: NewPromptEditor("")}
	env.feed("[200~")
	s.handleEscape()
	if !s.inPaste {
		t.Fatal("paste start not recognised")
	}
	// Inside a paste, ANSI codes are kept until the paste ends and then stripped; the
	// drag-and-drop quoting is removed.
	env.feed("[31m")
	s.handleKey(0x1b)
	for _, ch := range []byte("'/tmp/my dir/f.txt'") {
		s.handleKey(ch)
	}
	if s.ed.Text() != "" {
		t.Fatalf("text inserted before paste ended: %q", s.ed.Text())
	}
	env.feed("[201~")
	s.handleEscape()
	if s.inPaste {
		t.Fatal("paste end not recognised")
	}
	if want := "/tmp/my dir/f.txt"; s.ed.Text() != want {
		t.Errorf("pasted text = %q, want %q", s.ed.Text(), want)
	}

	// Enter inside a paste is a newline, not a submit.
	env.feed("[200~")
	s.handleEscape()
	for _, ch := range []byte("a\rb\nc") {
		if done, _, _, _ := s.handleKey(ch); done {
			t.Fatal("Enter inside a paste ended the input")
		}
	}
	env.feed("[201~")
	s.handleEscape()
	if want := "/tmp/my dir/f.txta\nb\nc"; s.ed.Text() != want {
		t.Errorf("multi-line paste = %q, want %q", s.ed.Text(), want)
	}
}

func TestPromptEditorRenderSequences(t *testing.T) {
	t.Run("single row", func(t *testing.T) {
		screen := newPTYScreen(t)
		NewPromptEditor("hello")
		screen.waitFor(t, "\r\x1b[J> hello\r\x1b[7C")
	})
	t.Run("wrapped text moves up and back", func(t *testing.T) {
		screen := newPTYScreen(t)
		ed := NewPromptEditor("")
		ed.SetWidth(10)
		ed.InsertRunes([]rune("0123456789abcdef"))
		want := "\r\x1b[J> 0123456789abcdef\r\x1b[8C"
		screen.waitFor(t, "\x1b[8C")
		if got := screen.String(); got != want {
			t.Fatalf("first render = %q, want %q", got, want)
		}
		if ed.renderedVisualRow != 1 || ed.renderedTotalRows != 2 {
			t.Errorf("rendered rows = %d/%d, want 1/2", ed.renderedVisualRow, ed.renderedTotalRows)
		}
		mark := screen.Len()
		ed.MoveLineStart()
		want = "\x1b[1A\r\x1b[J> 0123456789abcdef\x1b[1A\r\x1b[2C"
		screen.waitForAfter(t, mark, "\x1b[2C")
		if got := screen.String()[mark:]; got != want {
			t.Errorf("second render = %q, want %q", got, want)
		}
	})
	t.Run("multiline uses indent", func(t *testing.T) {
		screen := newPTYScreen(t)
		NewPromptEditor("ab\ncd")
		screen.waitFor(t, "\r\x1b[J> ab\r\r\n  cd\r\x1b[4C")
	})
	t.Run("not a terminal draws nothing but tracks layout", func(t *testing.T) {
		env := newMenuEnv(t)
		ed := NewPromptEditor("ab\ncd")
		if out := env.output(); out != "" {
			t.Errorf("wrote %q to a non-terminal", out)
		}
		if ed.renderedLine != 1 || ed.renderedLines != 1 || ed.renderedTotalRows != 2 {
			t.Errorf("layout state = %d/%d/%d", ed.renderedLine, ed.renderedLines, ed.renderedTotalRows)
		}
	})
}

func TestStartHaltListener(t *testing.T) {
	start := func(t *testing.T) (*ptyScreen, context.Context, func()) {
		screen := newPTYScreen(t)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return screen, ctx, StartHaltListener(cancel)
	}
	waitCancelled := func(t *testing.T, ctx context.Context) {
		t.Helper()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("context was not cancelled")
		}
	}

	t.Run("Esc cancels", func(t *testing.T) {
		screen, ctx, stop := start(t)
		defer stop()
		screen.Type(t, "\x1b")
		waitCancelled(t, ctx)
		screen.waitFor(t, "[halted by user (Esc)]")
	})
	t.Run("Ctrl+C cancels without raising a signal", func(t *testing.T) {
		screen, ctx, stop := start(t)
		defer stop()
		screen.Type(t, "\x03")
		waitCancelled(t, ctx)
		screen.waitFor(t, "[halted by user (Ctrl+C)]")
	})
	t.Run("arrow key sequences and other keys are ignored", func(t *testing.T) {
		screen, ctx, stop := start(t)
		defer stop()
		screen.Type(t, "\x1b[Aabc\x03")
		waitCancelled(t, ctx)
		screen.waitFor(t, "(Ctrl+C)")
		if out := screen.String(); strings.Contains(out, "(Esc)") {
			t.Errorf("arrow key halted the run: %q", out)
		}
	})
	t.Run("stop restores the terminal and leaves the context alone", func(t *testing.T) {
		screen, ctx, stop := start(t)
		if screen.canonical(t) {
			t.Error("listener should put the terminal in raw mode while running")
		}
		stop()
		if !screen.canonical(t) {
			t.Error("stop did not restore the terminal settings")
		}
		if ctx.Err() != nil {
			t.Error("stop must not cancel the context")
		}
	})
	t.Run("a key typed after stop is left for the next reader", func(t *testing.T) {
		// The listener polls stdin; once stop returns it must not read again, or it
		// swallows the first key meant for the prompt or menu after the turn.
		screen, _, stop := start(t)
		time.Sleep(20 * time.Millisecond) // let it settle into polling, as during a turn
		stop()
		screen.setRaw(t)
		screen.Type(t, "x")
		// Outlast the listener's 50ms poll, so a listener still running would take the key.
		time.Sleep(150 * time.Millisecond)
		got := make(chan byte, 1)
		go func() {
			b := make([]byte, 1)
			if n, err := os.Stdin.Read(b); err == nil && n == 1 {
				got <- b[0]
			}
		}()
		select {
		case b := <-got:
			if b != 'x' {
				t.Fatalf("read %q, want x", b)
			}
		case <-time.After(time.Second):
			screen.Type(t, "y") // release the reader goroutine so the PTY can close
			<-got
			t.Fatal("key typed after stop was consumed by the stopped listener")
		}
	})
	t.Run("no terminal gives a no-op stop", func(t *testing.T) {
		pipeStdin(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		StartHaltListener(cancel)()
		if ctx.Err() != nil {
			t.Error("context cancelled without a keypress")
		}
	})
}

// Ctrl+C on a real terminal must reach the menu and prompt as a key. With the
// tty's interrupt character left enabled the line discipline swallows it (and
// raises SIGINT, quitting the agent), so these would time out.
func TestCtrlCIsAKeyOnTerminal(t *testing.T) {
	await := func(t *testing.T, ch <-chan [3]any) [3]any {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(3 * time.Second):
			t.Fatal("Ctrl+C never reached the reader")
			return [3]any{}
		}
	}

	t.Run("menu cancels", func(t *testing.T) {
		screen := newPTYScreen(t)
		ch := make(chan [3]any, 1)
		go func() {
			it, ok, err := InteractiveSelect("pick", numberedItems(3), 0, nil, nil)
			ch <- [3]any{it, ok, err}
		}()
		screen.waitFor(t, "pick")
		screen.Type(t, "\x03")
		r := await(t, ch)
		if r[1].(bool) || r[2] != nil {
			t.Fatalf("got (%v, %v, %v), want cancelled", r[0], r[1], r[2])
		}
		if !screen.canonical(t) {
			t.Error("terminal mode not restored")
		}
	})

	t.Run("prompt clears text, then ignores Ctrl+C", func(t *testing.T) {
		screen := newPTYScreen(t)
		ch := make(chan [3]any, 1)
		go func() {
			in, action, err := ReadSeamlessInput(bufio.NewReader(os.Stdin), nil)
			ch <- [3]any{in, action, err}
		}()
		// Keys typed before raw mode is on would be handled by the line discipline:
		// Ctrl+C would become a signal and never reach the reader.
		screen.waitFor(t, "\x1b[?2004h")
		screen.Type(t, "draft")
		screen.waitFor(t, "draft")
		screen.Type(t, "\x03")
		screen.Type(t, "\x03")
		// An empty-prompt Ctrl+C is consumed silently: the prompt keeps reading keys.
		screen.Type(t, "hi\r")
		r := await(t, ch)
		if r[0] != "hi" || r[1] != ActionNone || r[2] != nil {
			t.Fatalf("got (%q, %v, %v), want \"hi\" submitted", r[0], r[1], r[2])
		}
		if strings.Contains(screen.String(), "^C") {
			t.Errorf("Ctrl+C echoed ^C: %q", screen.String())
		}
	})
}

// Input that arrives faster than it is handled (type-ahead, or a paste in a terminal
// without bracketed paste) is drawn once the queue drains, not once per character,
// which would cost a full redraw of the growing prompt per byte.
func TestQueuedInputIsRenderedOnce(t *testing.T) {
	screen := newPTYScreen(t)
	ch := make(chan string, 1)
	go func() {
		in, _, _ := ReadSeamlessInput(bufio.NewReader(os.Stdin), nil)
		ch <- in
	}()
	screen.waitFor(t, "\x1b[?2004h") // raw mode is on
	text := strings.Repeat("queued text ", 250)
	screen.Type(t, text+"\r")
	select {
	case got := <-ch:
		if got != strings.TrimSpace(text) {
			t.Fatalf("got %d chars, want %d", len(got), len(strings.TrimSpace(text)))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("input never submitted")
	}
	if renders := strings.Count(screen.String(), "\r\x1b[J"); renders > 30 {
		t.Errorf("prompt redrawn %d times for one burst of %d bytes", renders, len(text))
	}
}
