package terminal

import (
	"strings"
	"sync"
	"testing"
	"time"

	"hmj-agent/internal/terminal/theme"
)

// newTTYSpinner builds a spinner over an in-memory buffer with isTTY=true,
// exercising the real draw/erase/stop paths that are inert when `go test`
// runs with piped stdout.
func newTTYSpinner(label string) (*Spinner, *syncBuffer) {
	buf := &syncBuffer{}
	return NewSpinnerTo(label, buf, true), buf
}

type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func TestSpinnerDrawsFramesAndAdvances(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	s, buf := newTTYSpinner("thinking…")
	s.Start()

	// Cursor hidden on start.
	if out := buf.String(); !strings.Contains(out, "\x1b[?25l") {
		t.Errorf("expected cursor-hide sequence on Start, got %q", out)
	}

	// Let at least two ticks elapse so the frame advances.
	time.Sleep(280 * time.Millisecond)
	s.Stop()

	out := buf.String()
	if !strings.Contains(out, "thinking…") {
		t.Errorf("expected label in output, got %q", out)
	}
	if !strings.Contains(out, "\x1b[K") {
		t.Errorf("expected clear-to-EOL sequences, got %q", out)
	}
	if strings.Count(out, "\r\x1b[K") < 2 {
		t.Errorf("expected multiple redraws across ticks, got %q", out)
	}
}

func TestSpinnerStopErasesAndShowsCursor(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	s, buf := newTTYSpinner("working")
	s.Start()
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		s.Stop()
		s.Stop() // idempotent: must not block or double-close
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("repeated Stop blocked; Stop must be idempotent")
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\x1b[?25h") {
		t.Errorf("expected cursor restore at end of output, got %q", out)
	}
	// The final write after hide-cursor must be the erase, not a frame:
	// trailing frames would leave residue on the line.
	if idx := strings.LastIndex(out, "\x1b[?25h"); idx > 0 {
		tail := out[:idx]
		if !strings.HasSuffix(tail, "\r\x1b[K") {
			t.Errorf("expected line erase immediately before cursor restore, got %q", tail)
		}
	}
}

func TestSpinnerPauseSuppressesRedraws(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	s, buf := newTTYSpinner("paused check")
	s.Start()
	time.Sleep(120 * time.Millisecond)

	PauseActiveSpinner()
	before := strings.Count(buf.String(), "\r\x1b[K")
	time.Sleep(250 * time.Millisecond) // several ticks while paused
	after := strings.Count(buf.String(), "\r\x1b[K")

	if after != before {
		t.Errorf("paused spinner kept redrawing: %d draws before, %d after", before, after)
	}

	ResumeActiveSpinner()
	s.Stop()

	out := buf.String()
	if !strings.Contains(out, "\x1b[?25h") {
		t.Errorf("expected cursor restored after pause/resume/stop, got %q", out)
	}
}

// Pause racing a draw must never interleave erase/write: while the spinner
// is being paused from a foreign goroutine, every erase must be the last
// write on its line. Run with -race to validate lock discipline.
func TestSpinnerPauseVsDrawRace(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	s, buf := newTTYSpinner("race")
	s.Start()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				PauseActiveSpinner()
				time.Sleep(time.Millisecond)
				ResumeActiveSpinner()
			}
		}
	})

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	s.Stop()

	// While paused, no spinner frame may be written after the pause erase;
	// the buffer must never contain a draw immediately following a pause
	// erase without an intervening resume. Approximate check: output must
	// not end mid-line with a frame right after an erase.
	out := buf.String()
	if strings.Contains(out, "\x1b[K\x1b[K") {
		t.Errorf("double erase detected (erase raced a draw): %q", out)
	}
}

func TestSpinnerAccessibilityBadge(t *testing.T) {
	theme.SetAccessibility(true)
	defer theme.SetAccessibility(false)

	s, buf := newTTYSpinner("checking")
	s.Start()
	time.Sleep(150 * time.Millisecond)
	s.Stop()

	out := buf.String()
	if !strings.Contains(out, "[working]") {
		t.Errorf("expected accessibility badge, got %q", out)
	}
	if strings.ContainsAny(out, "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") {
		t.Errorf("accessibility mode must not emit braille frames, got %q", out)
	}
}

func TestSpinnerNonTTYIsInert(t *testing.T) {
	buf := &syncBuffer{}
	s := NewSpinnerTo("thinking…", buf, false)

	start := time.Now()
	s.Start()
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("Start on non-TTY should return immediately, took %v", time.Since(start))
	}

	s.Stop()
	s.Stop()

	// Nil receiver safety.
	var nilSpinner *Spinner
	nilSpinner.Start()
	nilSpinner.Stop()

	// A nil spinner must never register itself as the active one.
	PauseActiveSpinner()
	ResumeActiveSpinner()
	StopActiveSpinner()

	if buf.String() != "" {
		t.Errorf("non-TTY spinner wrote output: %q", buf.String())
	}
}

func TestSpinnerStopActiveSpinnerFromForeignGoroutine(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	s, buf := newTTYSpinner("halting")
	s.Start()
	time.Sleep(80 * time.Millisecond)

	// Mirrors the halt listener: Stop must be safe from a non-owner
	// goroutine and synchronously erase the line.
	done := make(chan struct{})
	go func() {
		StopActiveSpinner()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopActiveSpinner blocked from foreign goroutine")
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\r\x1b[K\x1b[?25h") {
		t.Errorf("expected erase + cursor restore, got %q", out)
	}

	// Registry must now be empty: a follow-up stop is a no-op.
	StopActiveSpinner()
}

// Accessibility mode is append-only: no carriage return, no erase, no cursor hiding, and
// one "[working] label" line at start followed by a line each report interval.
func TestSpinnerAccessibleIsAppendOnly(t *testing.T) {
	theme.SetAccessibility(true)
	theme.SetEnabled(true)
	accessibleReportInterval = 20 * time.Millisecond
	defer func() {
		theme.SetAccessibility(false)
		theme.SetEnabled(false)
		accessibleReportInterval = 15 * time.Second
	}()

	s, buf := newTTYSpinner("thinking…")
	s.Start()
	time.Sleep(90 * time.Millisecond)
	s.Stop()
	after := buf.String()
	time.Sleep(40 * time.Millisecond)

	if !strings.HasPrefix(after, "[working] thinking...\n") {
		t.Errorf("expected ASCII start line first, got %q", after)
	}
	if n := strings.Count(after, "[working] thinking... ("); n < 2 {
		t.Errorf("expected periodic progress lines, got %d in %q", n, after)
	}
	for _, bad := range []string{"\r", "\x1b", "\x1b[K", "\x1b[?25l", "\x1b[?25h", "⠋"} {
		if strings.Contains(after, bad) {
			t.Errorf("accessible spinner output must not contain %q: %q", bad, after)
		}
	}
	if !strings.HasSuffix(after, "\n") {
		t.Errorf("expected output to end on a full line, got %q", after)
	}
	if buf.String() != after {
		t.Errorf("spinner wrote after Stop: %q", buf.String())
	}
}

func TestSpinnerAccessiblePauseSkipsLines(t *testing.T) {
	theme.SetAccessibility(true)
	accessibleReportInterval = 15 * time.Millisecond
	defer func() {
		theme.SetAccessibility(false)
		accessibleReportInterval = 15 * time.Second
	}()

	s, buf := newTTYSpinner("paused")
	s.Start()
	PauseActiveSpinner()
	before := buf.String()
	time.Sleep(60 * time.Millisecond)
	if buf.String() != before {
		t.Errorf("paused accessible spinner printed: %q", buf.String())
	}
	ResumeActiveSpinner()
	s.Stop()
}
