package terminal

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"hmj-agent/v2/internal/terminal/theme"
)

// covLockedBuffer lets a spinner goroutine write while the test reads.
type covLockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *covLockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *covLockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitForCov polls buf until it contains want, failing the test after two seconds.
func waitForCov(t *testing.T, buf *covLockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in %q", want, buf.String())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSpinnerNilAndUnstartedAreSafe(t *testing.T) {
	var nilSpinner *Spinner
	nilSpinner.SetLabel("x")
	nilSpinner.Start()
	nilSpinner.Stop()

	var out covLockedBuffer
	s := NewSpinnerTo("idle", &out, true)
	s.Stop() // never started: no-op
	if out.String() != "" {
		t.Errorf("unstarted spinner wrote %q", out.String())
	}
}

func TestSpinnerSetLabelAndStartIsIdempotent(t *testing.T) {
	theme.SetEnabled(false)
	theme.SetAccessibility(false)
	var out covLockedBuffer
	s := NewSpinnerTo("first", &out, true)
	s.SetLabel("thinking…")
	s.Start()
	s.Start() // second start must not hide the cursor twice
	waitForCov(t, &out, "thinking…")
	s.Stop()

	got := out.String()
	if n := strings.Count(got, spinnerHideCursor); n != 1 {
		t.Errorf("cursor hidden %d times, want 1: %q", n, got)
	}
	if !strings.Contains(got, "thinking…") || strings.Contains(got, "first") {
		t.Errorf("label not updated before drawing: %q", got)
	}
	if !strings.HasSuffix(got, spinnerShowCursor) {
		t.Errorf("cursor not restored at end: %q", got)
	}
	if !strings.Contains(got, "\r\x1b[K") {
		t.Errorf("drawn line not erased on Stop: %q", got)
	}
}

func TestSpinnerAccessibleLabelIsASCII(t *testing.T) {
	theme.SetAccessibility(true)
	t.Cleanup(func() { theme.SetAccessibility(false) })
	prev := accessibleReportInterval
	accessibleReportInterval = time.Hour
	t.Cleanup(func() { accessibleReportInterval = prev })

	var out covLockedBuffer
	s := NewSpinnerTo("waiting… → model", &out, true)
	s.Start()
	s.Stop()
	got := out.String()
	if !strings.HasPrefix(got, "[working] waiting... -> model\n") {
		t.Errorf("accessible start line = %q", got)
	}
	if strings.Contains(got, spinnerHideCursor) {
		t.Errorf("append-only spinner hid the cursor: %q", got)
	}
}

func TestActiveSpinnerPauseResumeAndStop(t *testing.T) {
	theme.SetEnabled(false)
	theme.SetAccessibility(false)
	var out covLockedBuffer
	s := NewSpinnerTo("busy", &out, true)
	s.Start()
	t.Cleanup(StopActiveSpinner)
	waitForCov(t, &out, "busy")

	PauseActiveSpinner()
	paused := out.String()
	time.Sleep(2 * spinnerInterval)
	if out.String() != paused {
		t.Errorf("spinner drew while paused")
	}
	ResumeActiveSpinner()
	StopActiveSpinner()
	if !strings.Contains(out.String(), "busy") {
		t.Errorf("spinner never drew the label: %q", out.String())
	}
	// Stopped: the active pointer is cleared, so further calls are harmless.
	PauseActiveSpinner()
	ResumeActiveSpinner()
	StopActiveSpinner()
}

func TestAccessibleProgressStopIsIdempotent(t *testing.T) {
	var out covLockedBuffer
	stop := startProgress(&out, "download…", time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "[running] download… (") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	stop()
	n := len(out.String())
	time.Sleep(5 * time.Millisecond)
	if len(out.String()) != n {
		t.Error("progress line written after stop returned")
	}
	if !strings.Contains(out.String(), "[running] download… (") {
		t.Errorf("no progress line written: %q", out.String())
	}
}

func TestFilterItemsMatchesTextAndID(t *testing.T) {
	items := []SelectItem{
		{ID: "/models", Text: "/models  Switch AI model"},
		{ID: "/resume", Text: "/resume  Resume conversations"},
		{Text: "plain entry"},
	}
	if got := filterItems(items, ""); len(got) != 3 {
		t.Errorf("empty query returned %d items", len(got))
	}
	if got := filterItems(items, "  MODEL "); len(got) != 1 || got[0].ID != "/models" {
		t.Errorf("case/space-insensitive match = %+v", got)
	}
	if got := filterItems(items, "/RES"); len(got) != 1 || got[0].ID != "/resume" {
		t.Errorf("ID match = %+v", got)
	}
	if got := filterItems(items, "zzz"); len(got) != 0 {
		t.Errorf("no-match query returned %+v", got)
	}
	if got := filterItems(nil, "x"); got != nil {
		t.Errorf("nil items with a query = %v, want nil", got)
	}
	// The result is a copy: mutating it must not change the input.
	all := filterItems(items, "")
	all[0].Text = "changed"
	if items[0].Text == "changed" {
		t.Error("filterItems with empty query aliases its input")
	}
}
