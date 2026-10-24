package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
)

// TestThoughtsCollapseToOneLine checks that the model's reasoning is not printed and that one
// line saying how long it thought comes before the answer, in both views.
func TestThoughtsCollapseToOneLine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","delta":"weighing options"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"The answer"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`)
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
	}}
	run := func(accessible bool) string {
		theme.SetAccessibility(accessible)
		t.Cleanup(func() { theme.SetAccessibility(false) })
		runner := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
		conv := &session.Conversation{Model: config.ModelRef{Provider: "p", Model: "m"}}
		return captureOutput(t, func() {
			if err := runner.Run(context.Background(), conv, "question"); err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		accessible bool
		line       string
	}{{false, "▸ Thought for "}, {true, "> Thought for "}} {
		out := run(tc.accessible)
		line, answer := strings.Index(out, tc.line), strings.Index(out, "The answer")
		if line < 0 || answer < 0 || line > answer {
			t.Errorf("accessible %v: output = %q, want %q before the answer", tc.accessible, out, tc.line)
		}
		if strings.Contains(out, "weighing options") {
			t.Errorf("accessible %v: the reasoning was printed: %q", tc.accessible, out)
		}
	}
}

// TestThoughtDurationLine checks the line's wording: whole seconds, at least 1.
func TestThoughtDurationLine(t *testing.T) {
	for d, want := range map[time.Duration]string{200 * time.Millisecond: "▸ Thought for 1s\n", 8400 * time.Millisecond: "▸ Thought for 8s\n", 125 * time.Second: "▸ Thought for 2m05s\n"} {
		if got := theme.StripANSI(captureOutput(t, func() { printThoughtDuration(d) })); got != want {
			t.Errorf("%v: got %q, want %q", d, got, want)
		}
	}
}

// TestThoughtLineShowsTheLatestSentence checks the status line's text as reasoning streams in.
func TestThoughtLineShowsTheLatestSentence(t *testing.T) {
	tl := &thoughtLine{start: time.Now()}
	for _, step := range []struct{ chunk, want string }{
		{"**Planning the fix**", "Planning the fix"},
		{"\n\n", "Planning the fix"},
		{"The test fails on", "The test fails on"},
		{" retry. I should", "I should"},
		{" read retry.go.", "I should read retry.go."},
		{"\n\x1b[31m  Then   run\tit", "Then run it"},
	} {
		if got := tl.add(step.chunk); got != step.want {
			t.Errorf("after %q: got %q, want %q", step.chunk, got, step.want)
		}
	}
	tl.add(strings.Repeat("é", maxThoughtBuffer) + "\nlast words")
	if len(tl.buf) > maxThoughtBuffer || tl.add("") != "last words" {
		t.Errorf("buffer of %d bytes, line %q", len(tl.buf), tl.add(""))
	}
}

// TestThoughtDurationCountsFromTheRequest checks that reasoning is timed from the request, since
// a provider may send its summary of the reasoning all at once when the reasoning is done.
func TestThoughtDurationCountsFromTheRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(1600 * time.Millisecond) // the model reasons before anything is sent
		fmt.Fprint(w, `event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","delta":"all of it at once"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"The answer"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`)
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
	}}
	runner := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{Model: config.ModelRef{Provider: "p", Model: "m"}}
	out := captureOutput(t, func() {
		if err := runner.Run(context.Background(), conv, "question"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if !strings.Contains(out, "Thought for 2s") {
		t.Errorf("output = %q, want Thought for 2s", out)
	}
}

// TestThoughtsAfterTextStartANewLine checks that reasoning after answer text in the same step,
// whose last line has no line break yet, ends that line and leaves a blank line before its own.
func TestThoughtsAfterTextStartANewLine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"First line\nPartial"}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","delta":"more to weigh"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`)
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
	}}
	runner := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{Model: config.ModelRef{Provider: "p", Model: "m"}}
	out := captureOutput(t, func() {
		if err := runner.Run(context.Background(), conv, "question"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if !strings.Contains(out, "First line\nPartial\n\n▸ Thought for ") {
		t.Errorf("output = %q, want the text, a blank line, then the reasoning's line", out)
	}
}
