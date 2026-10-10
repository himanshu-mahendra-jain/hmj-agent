package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/tools"
)

// scriptedProvider is a mock OpenAI Responses endpoint. Each request takes the next
// queued reply; when the queue is empty it answers "ok". It records the request bodies.
type scriptedProvider struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  []http.HandlerFunc
	requests []string
}

func newScriptedProvider(t *testing.T, replies ...http.HandlerFunc) *scriptedProvider {
	t.Helper()
	p := &scriptedProvider{replies: replies}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.requests = append(p.requests, string(body))
		next := textReply("ok")
		if len(p.replies) > 0 {
			next, p.replies = p.replies[0], p.replies[1:]
		}
		p.mu.Unlock()
		next(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// Requests returns the request bodies received so far.
func (p *scriptedProvider) Requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

// config returns a config naming this provider "mock" with one model, "mock-model".
func (p *scriptedProvider) config(contextWindow int) config.Config {
	return config.Config{Providers: map[string]config.Provider{
		"mock": {BaseURL: p.srv.URL, API: "responses", Models: []config.Model{{ID: "mock-model", ContextWindow: contextWindow}}},
	}}
}

var mockModel = config.ModelRef{Provider: "mock", Model: "mock-model"}

// textReply streams text as the whole answer.
func textReply(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", text)
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n")
	}
}

// toolCallReply answers with one call to the named tool.
func toolCallReply(name, args string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"item_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":%q,\"arguments\":%q}}\n\n", name, args)
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n")
	}
}

// statusReply answers with a non-streaming HTTP error.
func statusReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// longConversation returns a conversation with turns user turns, enough to be condensed
// when turns is over session.DefaultMaxTurnsBeforeSummary.
func longConversation(turns int, replySize int) *session.Conversation {
	conv := &session.Conversation{ID: "1", Name: "long", Model: mockModel}
	conv.Messages = append(conv.Messages, chat.Message{Role: "system", Content: "system"})
	for i := range turns {
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("old request %d", i)},
			chat.Message{Role: "assistant", Content: fmt.Sprintf("old reply %d ", i) + strings.Repeat("x", replySize)})
	}
	return conv
}

// TestCondenseFailureLeavesTranscriptAndTurnGoesAhead checks that when the model cannot
// summarize the older turns, the user is told why, the transcript keeps every turn, and the
// turn itself still runs.
func TestCondenseFailureLeavesTranscriptAndTurnGoesAhead(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := []struct {
		name    string
		summary http.HandlerFunc
		want    string
	}{
		{"model calls a tool", toolCallReply("read_file", `{"path":"x"}`), "the model called a tool instead of writing the summary"},
		{"provider rejects the request", statusReply(http.StatusBadRequest, `{"error":{"message":"bad summary request"}}`), "bad summary request"},
		{"empty summary", textReply("   "), session.ErrEmptySummary.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newScriptedProvider(t, tc.summary, textReply("answer"))
			cfg := p.config(0)
			r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{&tools.ReadFileTool{}})
			conv := longConversation(session.DefaultMaxTurnsBeforeSummary+1, 10)

			out := captureOutput(t, func() {
				if err := r.Run(context.Background(), conv, "new request"); err != nil {
					t.Errorf("Run: %v", err)
				}
			})

			if !strings.Contains(out, "[could not condense earlier turns: ") || !strings.Contains(out, tc.want) {
				t.Errorf("output does not report the condense failure %q:\n%s", tc.want, out)
			}
			if !strings.Contains(out, "answer") {
				t.Errorf("the turn did not go ahead after the failed summary:\n%s", out)
			}
			if got := conv.UserMessageCount(); got != session.DefaultMaxTurnsBeforeSummary+2 {
				t.Errorf("user messages = %d, want every old turn kept plus the new one", got)
			}
			reqs := p.Requests()
			if len(reqs) != 2 || !strings.Contains(reqs[1], "old request 0") || !strings.Contains(reqs[1], "new request") {
				t.Errorf("want a summary request then the full turn, got %d requests", len(reqs))
			}
		})
	}
}

// TestCondenseCancelledPrintsNothing checks that a turn halted while the summary is being
// written is not reported as a condense failure: the halt reports itself.
func TestCondenseCancelledPrintsNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t)
	cfg := p.config(0)
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := longConversation(session.DefaultMaxTurnsBeforeSummary+1, 10)
	before := len(conv.Messages)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := captureOutput(t, func() { r.condense(ctx, conv) })

	if strings.Contains(out, "could not condense") {
		t.Errorf("a cancelled summary was reported as a failure: %q", out)
	}
	if len(conv.Messages) != before {
		t.Errorf("messages = %d, want %d: a cancelled summary must leave the transcript", len(conv.Messages), before)
	}
}

// TestFitContextWithinBudgetSendsNoSummaryRequest checks that a conversation well inside the
// model's context window is sent as it is, without asking for a summary.
func TestFitContextWithinBudgetSendsNoSummaryRequest(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t)
	cfg := p.config(1_000_000)
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := longConversation(3, 100)

	out := captureOutput(t, func() {
		if err := r.Run(context.Background(), conv, "next"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if n := len(p.Requests()); n != 1 {
		t.Errorf("requests = %d, want only the turn", n)
	}
	if strings.Contains(out, "condensed") || strings.Contains(out, "context window") {
		t.Errorf("a small conversation was condensed: %s", out)
	}
}

// TestFitContextSummaryFailureIsReported checks that a conversation over the context budget
// whose summary fails is reported, and the turn is still sent.
func TestFitContextSummaryFailureIsReported(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, statusReply(http.StatusBadRequest, `{"error":{"message":"summary refused"}}`), textReply("answer"))
	cfg := p.config(4_000)
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := longConversation(3, 8_000) // ~6k tokens, over 75% of 4k

	out := captureOutput(t, func() {
		if err := r.Run(context.Background(), conv, "next"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if !strings.Contains(out, "[could not condense earlier turns: ") || !strings.Contains(out, "summary refused") {
		t.Errorf("summary failure not reported:\n%s", out)
	}
	if !strings.Contains(out, "answer") || len(p.Requests()) != 2 {
		t.Errorf("turn not sent after the failed summary (%d requests):\n%s", len(p.Requests()), out)
	}
}
