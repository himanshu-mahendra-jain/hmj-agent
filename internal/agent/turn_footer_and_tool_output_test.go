package agent

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/diff"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
)

// TestTurnFooterReportsCacheAndTruncation checks a turn whose reply was cut off at the
// output token limit: the reasoning block is closed before the answer, the footer counts the
// cached prompt tokens, and a warning says the output was cut off.
func TestTurnFooterReportsCacheAndTruncation(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"weighing options\"}\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n")
		fmt.Fprint(w, "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},"+
			"\"usage\":{\"input_tokens\":1000,\"output_tokens\":50,\"total_tokens\":1050,\"prompt_details\":{\"cached_tokens\":800}}}}\n\n")
	})
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{ID: "1", Model: mockModel}

	out := captureOutput(t, func() {
		if err := r.Run(context.Background(), conv, "explain"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	thought, answer := strings.Index(out, "Thought for "), strings.Index(out, "partial answer")
	if thought < 0 || answer < thought {
		t.Fatalf("want the reasoning, then the answer:\n%q", out)
	}
	if !strings.Contains(out[thought:answer], "\n\n") {
		t.Errorf("the reasoning block is not closed before the answer: %q", out[thought:answer])
	}
	for _, want := range []string{"1,000 input", "800", "cached", "50 output", "[warning: output cut off by model maximum token limit]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestTurnFooterCountsAnthropicCacheReads checks that the footer counts the prompt-cache reads an
// Anthropic stream reports, which arrive in other usage fields than the OpenAI protocols use.
func TestTurnFooterCountsAnthropicCacheReads(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":900,\"cache_creation_input_tokens\":0}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	cfg := config.Config{Providers: map[string]config.Provider{
		"mock": {BaseURL: p.srv.URL, API: config.APIAnthropic, Models: []config.Model{{ID: "mock-model"}}},
	}}
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{ID: "1", Model: mockModel}

	out := captureOutput(t, func() {
		if err := r.Run(context.Background(), conv, "hello"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	for _, want := range []string{"1,000 input", "900", "cached"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestTurnFooterCachedMatchesTheInput checks that a multi-step turn shows the latest step's input
// with that step's cached tokens, not the cached tokens of every step added up, while the total
// still adds up every step.
func TestTurnFooterCachedMatchesTheInput(t *testing.T) {
	s := &turnStats{turnStart: time.Now()}
	for _, u := range []chat.TokenUsage{
		{PromptTokens: 2400, CachedTokens: 2300, CompletionTokens: 60, TotalTokens: 2460},
		{PromptTokens: 2700, CachedTokens: 2600, CompletionTokens: 120, TotalTokens: 2820},
	} {
		s.totalSteps++
		s.add(&u)
	}
	out := captureOutput(t, s.printFooter)
	for _, want := range []string{"2,700 input", "2,600", "cached", "5,280 total"} {
		if !strings.Contains(out, want) {
			t.Errorf("footer lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "4,900") {
		t.Errorf("footer adds up the cached tokens of every step: %q", out)
	}
}

// TestTurnFooterEndsWithTheTimeItWasDone checks that the footer's last part is the turn's
// duration followed by the local time it ended.
func TestTurnFooterEndsWithTheTimeItWasDone(t *testing.T) {
	s := &turnStats{turnStart: time.Now().Add(-90 * time.Second), totalSteps: 2}
	s.add(&chat.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	before := time.Now().Format("15:04")
	out := captureOutput(t, s.printFooter)
	after := time.Now().Format("15:04")

	m := regexp.MustCompile(`1m3\ds total, done at (\d\d:\d\d)\]`).FindStringSubmatch(theme.StripANSI(out))
	if m == nil {
		t.Fatalf("footer lacks the duration and done time: %q", out)
	}
	if m[1] != before && m[1] != after {
		t.Errorf("done at %s, want %s", m[1], before)
	}
}

// TestTurnFooterNamesFastMode checks that the footer names fast mode when every step ran in it,
// counts the steps when only some did, and says nothing about speed when none did.
func TestTurnFooterNamesFastMode(t *testing.T) {
	footer := func(speeds ...string) string {
		s := &turnStats{turnStart: time.Now()}
		for _, speed := range speeds {
			s.totalSteps++
			s.add(&chat.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Speed: speed})
		}
		return captureOutput(t, s.printFooter)
	}

	if out := footer("fast", "fast"); !strings.Contains(out, "fast mode") || strings.Contains(out, "fast mode in") {
		t.Errorf("all steps fast: footer = %q, want fast mode without a step count", out)
	}
	if out := footer("fast", "standard", "fast"); !strings.Contains(out, "fast mode in 2 of 3 steps") {
		t.Errorf("some steps fast: footer = %q, want fast mode in 2 of 3 steps", out)
	}
	for _, speeds := range [][]string{{"standard"}, {""}} {
		if out := footer(speeds...); strings.Contains(out, "fast") {
			t.Errorf("speeds %q: footer = %q, want no fast mode", speeds, out)
		}
	}
}

// TestUnknownToolCallIsAnsweredWithAnError checks that a call to a tool the runner does not
// have is answered with an error the model can read, and the turn carries on.
func TestUnknownToolCallIsAnsweredWithAnError(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, toolCallReply("teleport", `{"to":"mars"}`), textReply("sorry"))
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{ID: "1", Model: mockModel}

	var out string
	stderr := captureStderr(t, func() {
		out = captureOutput(t, func() {
			if err := r.Run(context.Background(), conv, "go to mars"); err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	})
	want := `error: unknown tool "teleport"`
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	var answered bool
	for _, m := range conv.Messages {
		if m.Role == "tool" && m.ToolCallID == "call_1" && m.Content == want {
			answered = true
		}
	}
	if !answered {
		t.Errorf("tool call not answered with %q: %+v", want, conv.Messages)
	}
	if reqs := p.Requests(); len(reqs) != 2 || !strings.Contains(reqs[1], "unknown tool") {
		t.Errorf("the error was not sent back to the model (%d requests)", len(reqs))
	}
	if !strings.Contains(out, "sorry") {
		t.Errorf("turn did not continue after the error: %q", out)
	}
}

// TestReadFileForDiffBestEffort checks that the file read for a diff preview never fails
// loudly: no path, an unresolvable home directory or a missing file just mean "no old text".
func TestReadFileForDiffBestEffort(t *testing.T) {
	t.Setenv("HOME", "")
	for _, path := range []string{"", "~/notes.txt", "/nonexistent/hmj/file"} {
		if text, ok := readFileForDiff(path); ok || text != "" {
			t.Errorf("readFileForDiff(%q) = %q, %v; want \"\", false", path, text, ok)
		}
	}
}

// TestEmptyToolResultShowsOnlyItsDuration checks that a tool that printed nothing still
// shows how long it ran, and nothing at all without a duration.
func TestEmptyToolResultShowsOnlyItsDuration(t *testing.T) {
	if out := captureOutput(t, func() { printExpandedToolResult("run_command", "\n", 2*time.Second) }); !strings.Contains(out, "(2.0s)") {
		t.Errorf("output = %q, want the duration", out)
	}
	if out := captureOutput(t, func() { printExpandedToolResult("run_command", "") }); out != "" {
		t.Errorf("output = %q, want nothing", out)
	}
}

// TestAccessibleDiffTruncationNoteIsASCII checks that the note under a truncated diff is
// plain ASCII in accessibility mode, so a screen reader reads it as text.
func TestAccessibleDiffTruncationNoteIsASCII(t *testing.T) {
	useAccessibility(t)
	var newText strings.Builder
	for i := range 50 {
		fmt.Fprintf(&newText, "line %d\n", i)
	}
	d := diff.Text("big.txt", "", newText.String(), diff.Options{MaxLines: 10})
	out := captureOutput(t, func() { printDiff(d) })
	if !strings.Contains(out, "more changed lines not shown") {
		t.Fatalf("no truncation note:\n%s", out)
	}
	assertAppendOnly(t, out)
}

// TestReasoningBeforeToolCallIsClosed checks that a step that only reasons and then calls a
// tool closes its reasoning block before the tool call is printed.
func TestReasoningBeforeToolCallIsClosed(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"need a listing\"}\n\n")
		toolCallReply("teleport", `{}`)(w, r)
	}, textReply("done"))
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{ID: "1", Model: mockModel}

	var out string
	captureStderr(t, func() {
		out = captureOutput(t, func() {
			if err := r.Run(context.Background(), conv, "list"); err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	})
	thought, call := strings.Index(out, "Thought for "), strings.Index(out, "teleport")
	if thought < 0 || call < thought {
		t.Fatalf("want the reasoning, then the tool call:\n%q", out)
	}
	if !strings.Contains(out[thought:call], "\n\n") {
		t.Errorf("reasoning block not closed before the tool call: %q", out[thought:call])
	}
}

// TestFailedRequestIsRecordedInTheTranscript checks that a request the provider rejects ends
// the turn with the error, which is also kept in the conversation so the transcript shows
// what happened.
func TestFailedRequestIsRecordedInTheTranscript(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, statusReply(http.StatusBadRequest, `{"error":{"message":"context too long"}}`))
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{ID: "1", Model: mockModel}

	var err error
	captureOutput(t, func() { err = r.Run(context.Background(), conv, "hi") })
	if err == nil || !strings.Contains(err.Error(), "context too long") {
		t.Fatalf("Run err = %v, want the provider's message", err)
	}
	last := conv.Messages[len(conv.Messages)-1]
	if last.Role != "assistant" || !strings.HasPrefix(last.Content, "[error: ") || !strings.Contains(last.Content, "context too long") {
		t.Errorf("last message = %+v, want the recorded error", last)
	}
}

// TestConversationHistorySeparatesTurns checks that the replayed history leaves out the
// system prompt and puts a blank line before each prompt that follows an answer.
func TestConversationHistorySeparatesTurns(t *testing.T) {
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "system", Content: "SYSTEM PROMPT"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "one"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "two"},
	}}
	out := theme.StripANSI(captureOutput(t, func() { PrintConversationHistory(conv) }))
	if !regexp.MustCompile(`one\n\s*\n> you: second`).MatchString(out) {
		t.Errorf("no blank line before the second prompt:\n%q", out)
	}
	if strings.Contains(out, "SYSTEM PROMPT") {
		t.Errorf("system prompt was printed: %q", out)
	}
}
