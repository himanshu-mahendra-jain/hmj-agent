package terminal

import (
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"hmj-agent/v2/internal/terminal/theme"
)

// lockedBuffer is a goroutine-safe sink for the ticker's writes.
type lockedBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func TestStartProgressPeriodicLinesThenStop(t *testing.T) {
	buf := &lockedBuffer{}
	stop := startProgress(buf, "run_command", 10*time.Millisecond)
	time.Sleep(45 * time.Millisecond)
	stop()

	out := buf.String()
	if n := strings.Count(out, "[running] run_command ("); n < 2 {
		t.Fatalf("expected periodic progress lines, got %d in %q", n, out)
	}
	if strings.ContainsAny(out, "\r\x1b") {
		t.Errorf("progress lines must be append-only, got %q", out)
	}

	// Nothing may be written once stop has returned.
	time.Sleep(30 * time.Millisecond)
	if after := buf.String(); after != out {
		t.Errorf("ticker wrote after stop: %q", after[len(out):])
	}
	stop() // idempotent
}

func TestStartProgressStopsGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	stop := startProgress(io.Discard, "fast", time.Millisecond)
	stop()
	stop()

	// stop waits for the ticker goroutine, so the count must be back to where it started.
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if now := runtime.NumGoroutine(); now > before {
		t.Errorf("goroutines before=%d after stop=%d: ticker leaked", before, now)
	}
}

func TestStartAccessibleProgressOnlyInAccessibilityMode(t *testing.T) {
	prev := accessibleReportInterval
	accessibleReportInterval = 5 * time.Millisecond
	defer func() { accessibleReportInterval = prev }()

	setAccessible(t)
	var stop func()
	out := captureStdout(t, func() {
		stop = StartAccessibleProgress("edit_file", 0)
		time.Sleep(30 * time.Millisecond)
		stop()
	})
	if !strings.Contains(out, "[running] edit_file (") {
		t.Errorf("accessible mode should print progress lines, got %q", out)
	}

	theme.SetAccessibility(false)
	out = captureStdout(t, func() {
		stop = StartAccessibleProgress("edit_file", 0)
		time.Sleep(30 * time.Millisecond)
		stop()
	})
	if out != "" {
		t.Errorf("progress must be silent outside accessibility mode, got %q", out)
	}
}
