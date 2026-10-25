package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/session"
)

// TestFailedStepStillPrintsTheTurnFooter checks that a turn whose second request fails still
// prints the footer, counting the step that was paid for, before Run returns the error.
func TestFailedStepStillPrintsTheTurnFooter(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t,
		toolCallReply("teleport", `{"to":"mars"}`),
		statusReply(http.StatusBadRequest, `{"error":{"message":"prompt is too long"}}`))
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{ID: "1", Model: mockModel}

	var out string
	var err error
	captureStderr(t, func() {
		out = captureOutput(t, func() { err = r.Run(context.Background(), conv, "go to mars") })
	})
	if err == nil || !strings.Contains(err.Error(), "prompt is too long") {
		t.Fatalf("Run err = %v, want the provider's message", err)
	}
	for _, want := range []string{"tokens:", "10 input", "2 output"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestFailedFirstStepPrintsNoFooter checks that a turn whose only request failed prints no
// footer, as no step reported usage.
func TestFailedFirstStepPrintsNoFooter(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, statusReply(http.StatusBadRequest, `{"error":{"message":"bad"}}`))
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{ID: "1", Model: mockModel}

	out := captureOutput(t, func() { _ = r.Run(context.Background(), conv, "hi") })
	if strings.Contains(out, "tokens:") {
		t.Errorf("footer printed without usage:\n%s", out)
	}
}

// TestTurnUpdatesTheConversationTime checks that a turn marks when the conversation last
// changed, whether it completes or its request fails.
func TestTurnUpdatesTheConversationTime(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, textReply("hello"), statusReply(http.StatusBadRequest, `{"error":{"message":"bad"}}`))
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{ID: "1", Model: mockModel}

	for _, prompt := range []string{"hi", "fails"} {
		before := time.Now()
		conv.UpdatedAt = time.Time{}
		captureStderr(t, func() { captureOutput(t, func() { _ = r.Run(context.Background(), conv, prompt) }) })
		if conv.UpdatedAt.Before(before) {
			t.Errorf("after the %q turn UpdatedAt = %v, want at least %v", prompt, conv.UpdatedAt, before)
		}
	}
}

// TestLiveStatusShowsTheTurnSoFar checks the status line's text: nothing before usage, then the
// latest prompt, the output and its speed, with the step once the turn has more than one.
func TestLiveStatusShowsTheTurnSoFar(t *testing.T) {
	s := &turnStats{totalSteps: 1}
	if got := s.liveStatus(); got != "" {
		t.Errorf("before usage: %q, want empty", got)
	}
	s.add(&chat.TokenUsage{PromptTokens: 412_345, CompletionTokens: 2_000, TTFT: time.Second, Duration: 21 * time.Second})
	if got, want := s.liveStatus(), "412K in · 2.0K out · 100 tokens/s"; got != want {
		t.Errorf("after step 1: %q, want %q", got, want)
	}
	s.totalSteps = 2
	if got, want := s.liveStatus(), "step 2 · 412K in · 2.0K out · 100 tokens/s"; got != want {
		t.Errorf("at step 2: %q, want %q", got, want)
	}
	s = &turnStats{totalSteps: 3}
	s.add(&chat.TokenUsage{PromptTokens: 50})
	if got, want := s.liveStatus(), "step 3 · 50 in · 0 out"; got != want {
		t.Errorf("without output: %q, want %q", got, want)
	}
}

// TestFormatCompactNumber checks the narrow number format, which rounds down so a count never
// shows more than it is.
func TestFormatCompactNumber(t *testing.T) {
	for n, want := range map[int]string{
		0: "0", 999: "999", 1_000: "1.0K", 9_999: "9.9K", 10_000: "10K",
		412_345: "412K", 999_999: "999K", 1_000_000: "1.0M", 1_250_000: "1.2M",
	} {
		if got := FormatCompactNumber(n); got != want {
			t.Errorf("FormatCompactNumber(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestProgressLineIsOneCleanLine checks that a tool's output line is made safe for the status
// line: escape sequences and control characters go, and whitespace runs become one space.
func TestProgressLineIsOneCleanLine(t *testing.T) {
	got := progressLine("\x1b[32m> Task\x1b[0m\t:sms:compileDebugKotlin  \x07UP-TO-DATE")
	if want := "> Task :sms:compileDebugKotlin UP-TO-DATE"; got != want {
		t.Errorf("progressLine = %q, want %q", got, want)
	}
}
