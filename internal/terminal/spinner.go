package terminal

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hmj-agent/v2/internal/terminal/theme"
)

// spinnerFrames is the braille dot cycle used for the animation.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	// spinnerInterval is the redraw cadence; fast enough to feel alive, slow
	// enough to stay invisible next to real streaming output.
	spinnerInterval = 100 * time.Millisecond

	spinnerHideCursor = "\x1b[?25l"
	spinnerShowCursor = "\x1b[?25h"
)

// accessibleReportInterval is how often accessibility mode appends a progress line.
var accessibleReportInterval = 15 * time.Second

// Spinner is a lightweight single-line activity indicator drawn at the start
// of a turn, before the first streamed token arrives. It gives immediate
// feedback that the submitted prompt was accepted and the model is working —
// critical when network latency or retry backoff delays time-to-first-token.
//
// The spinner degrades gracefully:
//   - When stdout is not a TTY (pipes, files, `go test`, CI), or TERM=dumb,
//     it draws nothing and every call is effectively free.
//   - In accessibility mode it is append-only: it prints `[working] label` when
//     it starts and a new `[working] label (Ns)` line every 15 seconds. It never
//     redraws, erases or hides the cursor, so screen readers hear each line once.
//
// All terminal writes (draw, erase, cursor show/hide) happen while holding
// the internal mutex, so retry notices from the chat client and halt messages
// from the key listener can safely pause or stop the spinner from their own
// goroutines without interleaving. Stop is synchronous: it waits for the draw
// goroutine to erase the line before returning, guaranteeing a clean hand-off
// to subsequent writers.
type Spinner struct {
	label string
	// status and detail follow the label on the drawn line: the turn's progress, and the
	// latest output of a running tool.
	status string
	detail string
	out    io.Writer
	isTTY  bool

	mu     sync.Mutex
	active bool
	paused bool
	drawn  bool
	// appendOnly is fixed at Start: accessibility mode prints lines instead of redrawing.
	appendOnly bool
	frame      int
	started    time.Time
	stopCh     chan struct{}
	doneCh     chan struct{}
}

// activeSpinner tracks the currently running spinner so other writers that
// print on separate goroutines (retry notices, halt messages) can stop or
// temporarily clear the line before emitting their own output.
var activeSpinner atomic.Pointer[Spinner]

// NewSpinner creates a spinner writing to stdout with the given label
// (e.g. "thinking…").
func NewSpinner(label string) *Spinner {
	return NewSpinnerTo(label, os.Stdout, isInteractiveStdout())
}

// NewSpinnerTo builds a spinner writing to w. The isTTY flag gates every
// write; tests inject a buffer with isTTY=true to exercise the live draw
// loop, which never runs under `go test` where stdout is a pipe.
func NewSpinnerTo(label string, w io.Writer, isTTY bool) *Spinner {
	return &Spinner{label: label, out: w, isTTY: isTTY}
}

// isInteractiveStdout reports whether animated output may be drawn on stdout.
// TERM=dumb terminals print escape sequences literally, so they are excluded
// even though their TCGETS ioctl succeeds.
func isInteractiveStdout() bool {
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return cachedIsTerminal(os.Stdout.Fd())
}

// SetLabel updates the spinner message while running.
func (s *Spinner) SetLabel(label string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.label = label
	s.mu.Unlock()
}

// SetStatus sets the progress shown after the label and elapsed time, such as the turn's step
// and token counts. Empty shows none.
func (s *Spinner) SetStatus(status string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

// SetDetail sets the text shown last on the line, such as a running command's latest output
// line. Empty shows none. The line is cut to the terminal width, so detail must be one line.
func (s *Spinner) SetDetail(detail string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.detail = detail
	s.mu.Unlock()
}

// Start begins the animation on a background goroutine. It is idempotent:
// calling Start on an already-running spinner is a no-op.
func (s *Spinner) Start() {
	if s == nil || !s.isTTY {
		return
	}

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return
	}
	s.active = true
	s.paused = false
	s.drawn = false
	s.frame = 0
	s.started = time.Now()
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.appendOnly = theme.Accessibility()
	if s.appendOnly {
		fmt.Fprintf(s.out, "[working] %s\n", theme.ASCII(s.label))
	}
	s.mu.Unlock()

	activeSpinner.Store(s)
	if !s.appendOnly {
		fmt.Fprint(s.out, spinnerHideCursor)
	}
	go s.loop()
}

// Stop halts the animation, erases the drawn line and restores the cursor,
// blocking until the erase has completed so subsequent output starts on a
// clean line. In accessibility mode nothing is erased: Stop only ends the
// progress lines. Safe to call multiple times, on a never-started spinner, or on
// a nil receiver.
func (s *Spinner) Stop() {
	if s == nil {
		return
	}

	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.active = false
	stopCh, doneCh := s.stopCh, s.doneCh
	s.mu.Unlock()

	if cur := activeSpinner.Load(); cur == s {
		activeSpinner.Store(nil)
	}

	close(stopCh)
	<-doneCh
}

// loop redraws on every tick until stopped.
func (s *Spinner) loop() {
	defer close(s.doneCh)
	if s.appendOnly {
		s.loopAppend()
		return
	}

	ticker := time.NewTicker(spinnerInterval)
	defer ticker.Stop()

	s.draw()
	for {
		select {
		case <-s.stopCh:
			s.mu.Lock()
			s.eraseLocked()
			s.mu.Unlock()
			fmt.Fprint(s.out, spinnerShowCursor)
			return
		case <-ticker.C:
		}

		s.mu.Lock()
		if !s.paused {
			s.frame = (s.frame + 1) % len(spinnerFrames)
		}
		paused := s.paused
		s.mu.Unlock()
		if !paused {
			// draw re-validates active/paused under the lock, closing the
			// gap between the check above and the actual terminal write.
			s.draw()
		}
	}
}

// loopAppend prints a progress line every accessibleReportInterval until stopped. It
// holds the mutex for each write, so pauses and stops from other goroutines stay ordered.
func (s *Spinner) loopAppend() {
	ticker := time.NewTicker(accessibleReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}

		s.mu.Lock()
		if s.active && !s.paused {
			fmt.Fprintf(s.out, "[working] %s (%ds)\n", theme.ASCII(s.label), s.elapsedSeconds())
		}
		s.mu.Unlock()
	}
}

// draw renders one frame. It holds the mutex across the terminal write so a
// concurrent PauseActiveSpinner can never interleave its erase between the
// state check and the output. Append-only spinners never draw.
func (s *Spinner) draw() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.active || s.paused || s.appendOnly {
		return
	}

	line := s.renderLocked()
	s.drawn = true
	fmt.Fprintf(s.out, "\r\x1b[K%s", line)
}

// spinnerWidth is the number of columns the drawn line may use. A line that wraps cannot be
// erased with a carriage return, so the line is cut to fit. Tests replace it.
var spinnerWidth = rowWidth

// renderLocked builds the line content, cut to spinnerWidth: when the status and detail do not
// both fit, the status is left out. Caller must hold s.mu.
func (s *Spinner) renderLocked() string {
	frame := spinnerFrames[s.frame]
	elapsed := "(" + FormatElapsed(s.elapsedSeconds()) + ")"
	var tail strings.Builder
	for _, part := range []string{s.status, s.detail} {
		if part != "" {
			tail.WriteString(" · " + part)
		}
	}
	room := spinnerWidth() - VisibleWidth(frame) - 1
	head := VisibleWidth(s.label) + 1 + VisibleWidth(elapsed)
	if s.detail != "" && head+VisibleWidth(tail.String()) > room {
		// The detail is what changes while a tool runs, so the status gives way to it.
		tail.Reset()
		tail.WriteString(" · " + s.detail)
	}
	if head > room {
		return fmt.Sprintf("%s %s", theme.Faint(frame), theme.Muted(clipEnd(s.label+" "+elapsed, room)))
	}
	line := fmt.Sprintf("%s %s %s", theme.Faint(frame), theme.Muted(s.label), theme.Faint(elapsed))
	if rest := clipEnd(tail.String(), room-head); rest != "" {
		line += theme.Faint(rest)
	}
	return line
}

// FormatElapsed writes whole seconds as 42s, 2m05s or 1h02m, for the status line and for
// durations of a minute and more elsewhere.
func FormatElapsed(sec int) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
	}
}

// elapsedSeconds is the whole seconds since Start; caller must hold s.mu.
func (s *Spinner) elapsedSeconds() int {
	return int(time.Since(s.started).Truncate(time.Second).Seconds())
}

// eraseLocked clears the drawn line without moving the cursor row.
// Caller must hold s.mu.
func (s *Spinner) eraseLocked() {
	if s.drawn {
		fmt.Fprint(s.out, "\r\x1b[K")
		s.drawn = false
	}
}

// PauseActiveSpinner erases the running spinner's line (if any) and suspends
// redraws, letting another goroutine print without interleaving. Call
// ResumeActiveSpinner afterwards. No-op when no spinner is running.
func PauseActiveSpinner() {
	if s := activeSpinner.Load(); s != nil {
		s.mu.Lock()
		s.paused = true
		s.eraseLocked()
		s.mu.Unlock()
	}
}

// ResumeActiveSpinner un-suspends redraws after PauseActiveSpinner and
// repaints the line immediately. No-op when no spinner is running.
func ResumeActiveSpinner() {
	if s := activeSpinner.Load(); s != nil {
		s.mu.Lock()
		s.paused = false
		s.mu.Unlock()
		s.draw()
	}
}

// StopActiveSpinner synchronously stops and erases the running spinner, if
// any. Used by the halt listener so an aborted turn tears the indicator down
// immediately instead of leaving it paused until agent.Run notices the
// cancellation.
func StopActiveSpinner() {
	if s := activeSpinner.Load(); s != nil {
		s.Stop()
	}
}
