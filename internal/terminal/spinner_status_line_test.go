package terminal

import (
	"strings"
	"testing"
	"time"

	"hmj-agent/v2/internal/terminal/theme"
)

// renderAt draws s's line as if the terminal were width columns wide.
func renderAt(t *testing.T, s *Spinner, width int) string {
	t.Helper()
	old := spinnerWidth
	spinnerWidth = func() int { return width }
	t.Cleanup(func() { spinnerWidth = old })
	s.mu.Lock()
	defer s.mu.Unlock()
	return theme.StripANSI(s.renderLocked())
}

// TestSpinnerLineShowsStatusAndDetail checks the order of the line's parts: frame, label,
// elapsed time, then the status and the detail, each after a separator.
func TestSpinnerLineShowsStatusAndDetail(t *testing.T) {
	s := NewSpinnerTo("run_command", &strings.Builder{}, true)
	s.started = time.Now().Add(-134 * time.Second)
	if got, want := renderAt(t, s, 120), "⠋ run_command (2m14s)"; got != want {
		t.Errorf("bare line = %q, want %q", got, want)
	}
	s.SetStatus("step 38 · 412K in")
	s.SetDetail("> Task :sms:compileDebugKotlin")
	if got, want := renderAt(t, s, 120), "⠋ run_command (2m14s) · step 38 · 412K in · > Task :sms:compileDebugKotlin"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	s.SetStatus("")
	if got, want := renderAt(t, s, 120), "⠋ run_command (2m14s) · > Task :sms:compileDebugKotlin"; got != want {
		t.Errorf("without status = %q, want %q", got, want)
	}
}

// TestSpinnerLineFitsTheTerminal checks that the line never wraps, which would leave a copy of
// it on screen: the status and detail are cut first, then the label.
func TestSpinnerLineFitsTheTerminal(t *testing.T) {
	s := NewSpinnerTo("run_command", &strings.Builder{}, true)
	s.started = time.Now()
	s.SetStatus("step 2")
	s.SetDetail(strings.Repeat("界", 200)) // two columns each
	for _, width := range []int{80, 30, 22, 12, 3} {
		got := renderAt(t, s, width)
		if w := VisibleWidth(got); w > width {
			t.Errorf("width %d: line is %d columns: %q", width, w, got)
		}
		if !strings.HasPrefix(got, "⠋ ") {
			t.Errorf("width %d: frame missing: %q", width, got)
		}
	}
	if got := renderAt(t, s, 30); !strings.HasPrefix(got, "⠋ run_command (0s) · 界") || !strings.HasSuffix(got, "…") {
		t.Errorf("width 30: want the head whole and the detail cut, got %q", got)
	}
	if got := renderAt(t, s, 12); got != "⠋ run_comma…" {
		t.Errorf("width 12: got %q, want the label cut", got)
	}
}

// TestSpinnerLineKeepsTheDetailOverTheStatus checks that on a terminal too narrow for both, the
// running tool's output stays and the turn's status gives way.
func TestSpinnerLineKeepsTheDetailOverTheStatus(t *testing.T) {
	s := NewSpinnerTo("run_command", &strings.Builder{}, true)
	s.started = time.Now().Add(-134 * time.Second)
	s.SetStatus("step 38 · 412K in · 21K out · 68 tokens/s")
	s.SetDetail("> Task :sms:compileDebugKotlin")
	if got, want := renderAt(t, s, 80), "⠋ run_command (2m14s) · > Task :sms:compileDebugKotlin"; got != want {
		t.Errorf("width 80: %q, want %q", got, want)
	}
	if got := renderAt(t, s, 120); !strings.Contains(got, "· step 38 ·") || !strings.HasSuffix(got, "· > Task :sms:compileDebugKotlin") {
		t.Errorf("width 120: want the status and the detail, got %q", got)
	}
	s.SetDetail("")
	if got, want := renderAt(t, s, 50), "⠋ run_command (2m14s) · step 38 · 412K in · 21K o…"; got != want {
		t.Errorf("width 50 without detail: %q, want %q", got, want)
	}
}

// TestFormatElapsed checks the elapsed time format at each unit's edges.
func TestFormatElapsed(t *testing.T) {
	for sec, want := range map[int]string{0: "0s", 59: "59s", 60: "1m00s", 134: "2m14s", 3599: "59m59s", 3600: "1h00m", 7380: "2h03m"} {
		if got := FormatElapsed(sec); got != want {
			t.Errorf("FormatElapsed(%d) = %q, want %q", sec, got, want)
		}
	}
}
