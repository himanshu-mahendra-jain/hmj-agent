package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/tools"
)

// scriptedProvider is a mock OpenAI Responses endpoint. Each request takes the next
// queued reply; when the queue is empty it answers "ok". A request to the compact endpoint takes
// the next compact reply instead, and when there is none gets a compacted window. It records the
// request bodies.
type scriptedProvider struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  []http.HandlerFunc
	compact  []http.HandlerFunc
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
		if strings.HasSuffix(r.URL.Path, "/compact") {
			next = compactReply("ENC")
			if len(p.compact) > 0 {
				next, p.compact = p.compact[0], p.compact[1:]
			}
		} else if len(p.replies) > 0 {
			next, p.replies = p.replies[0], p.replies[1:]
		}
		p.mu.Unlock()
		next(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// QueueCompact appends replies for the next compact requests.
func (p *scriptedProvider) QueueCompact(replies ...http.HandlerFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compact = append(p.compact, replies...)
}

// Requests returns the request bodies received so far.
func (p *scriptedProvider) Requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

// config returns a config naming this provider "mock" with one model, "mock-model". api is the
// protocol, "responses" when empty.
func (p *scriptedProvider) config(api ...string) config.Config {
	wire := "responses"
	if len(api) > 0 {
		wire = api[0]
	}
	return config.Config{Providers: map[string]config.Provider{
		"mock": {BaseURL: p.srv.URL, API: wire, Models: []config.Model{{ID: "mock-model"}}},
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

// compactReply answers a compact request with a window holding one compaction item.
func compactReply(encrypted string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"output":[{"type":"compaction","encrypted_content":%q}]}`, encrypted)
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

// largeConversation returns a conversation of a few turns whose provider reported a prompt over
// session.CompactAtTokens.
func largeConversation() *session.Conversation {
	conv := &session.Conversation{ID: "1", Name: "large", Model: mockModel}
	conv.Messages = append(conv.Messages, chat.Message{Role: "system", Content: "system"})
	for i := range 3 {
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("old request %d", i)},
			chat.Message{Role: "assistant", Content: fmt.Sprintf("old reply %d", i)})
	}
	conv.SetPromptTokens(session.CompactAtTokens + 1)
	return conv
}

// TestCompactionFailureLeavesTheConversation checks that when the conversation cannot be
// compacted, the user is told why and the conversation keeps every message, so it is tried again
// after the next turn. A provider without the compact endpoint has the model write a summary,
// whose failures are reported the same way.
func TestCompactionFailureLeavesTheConversation(t *testing.T) {
	t.Chdir(t.TempDir())
	notFound := statusReply(http.StatusNotFound, `{"error":{"message":"no such endpoint"}}`)
	cases := []struct {
		name     string
		compact  http.HandlerFunc
		summary  http.HandlerFunc
		requests int
		want     string
	}{
		{"compaction rejected", statusReply(http.StatusUnauthorized, `{"error":{"message":"bad key"}}`), nil, 1, "bad key"},
		{"no compaction item", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"output":[]}`) }, nil, 1, "no compaction item"},
		{"summary calls a tool", notFound, toolCallReply("read_file", `{"path":"x"}`), 2, "the model called a tool instead of writing the summary"},
		{"summary rejected", notFound, statusReply(http.StatusBadRequest, `{"error":{"message":"bad summary request"}}`), 2, "bad summary request"},
		{"empty summary", notFound, textReply("   "), 2, chat.ErrEmptySummary.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newScriptedProvider(t)
			p.QueueCompact(tc.compact)
			if tc.summary != nil {
				p.replies = append(p.replies, tc.summary)
			}
			cfg := p.config()
			r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{&tools.ReadFileTool{}})
			r.Client.RetryConfig = chat.RetryConfig{}
			conv := largeConversation()
			before := len(conv.Messages)

			out := captureOutput(t, func() { r.StartCompaction(context.Background(), conv).Finish() })

			if !strings.Contains(out, "[could not compact the conversation: ") || !strings.Contains(out, tc.want) {
				t.Errorf("output does not report the failure %q:\n%s", tc.want, out)
			}
			if len(conv.Messages) != before || !conv.NeedsCompaction() {
				t.Errorf("the conversation changed: %d messages, want %d still due for compaction", len(conv.Messages), before)
			}
			if n := len(p.Requests()); n != tc.requests {
				t.Errorf("%d requests, want %d", n, tc.requests)
			}
		})
	}
}

// TestCompactionStoppedLeavesTheConversation checks that a compaction stopped before it ends is
// reported as stopped, not as a failure, and changes nothing.
func TestCompactionStoppedLeavesTheConversation(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t)
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := largeConversation()
	before := len(conv.Messages)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := captureOutput(t, func() { r.StartCompaction(ctx, conv).Finish() })

	if strings.Contains(out, "could not compact") || !strings.Contains(out, "compaction stopped") {
		t.Errorf("a stopped compaction was not reported as stopped: %q", out)
	}
	if len(conv.Messages) != before {
		t.Errorf("messages = %d, want %d", len(conv.Messages), before)
	}
}

// TestCompactionWarnsWhenHistoryIsNotKept checks that a compaction whose replaced turns cannot be
// written for /history is applied and reported as done, followed by a warning.
func TestCompactionWarnsWhenHistoryIsNotKept(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t)
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := largeConversation()
	if err := os.WriteFile("file", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	conv.HistoryPath = filepath.Join("file", "c.jsonl")

	out := captureOutput(t, func() { r.StartCompaction(context.Background(), conv).Finish() })

	if !strings.Contains(out, "[compacted the conversation:") || !strings.Contains(out, "[the earlier turns were not kept for /history:") {
		t.Errorf("output should report the compaction and the lost turns:\n%s", out)
	}
	if !conv.IsCompacted() {
		t.Error("the conversation was not compacted")
	}
}

// TestNoCompactionUnderTheLimit checks that a conversation at or under the limit is not
// compacted, however many turns it has.
func TestNoCompactionUnderTheLimit(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t)
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := largeConversation()
	for i := range 40 {
		conv.Messages = append(conv.Messages, chat.Message{Role: "user", Content: fmt.Sprint(i)}, chat.Message{Role: "assistant", Content: "a"})
	}
	conv.SetPromptTokens(session.CompactAtTokens - 100)

	if c := r.StartCompaction(context.Background(), conv); c != nil {
		c.Finish()
		t.Error("a conversation under the limit was compacted")
	}
	if n := len(p.Requests()); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

// TestHaltedToolRoundOverTheLimitIsCompacted checks that a conversation whose last turn was
// halted in a tool round, which a halt closes with placeholder results, is compacted like any
// other once it is over the limit.
func TestHaltedToolRoundOverTheLimitIsCompacted(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t)
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{&tools.ReadFileTool{}})
	conv := largeConversation()
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "read it"},
		chat.Message{Role: "assistant", Blocks: []chat.Block{{Type: "thinking", Text: "t", Signature: "s"}},
			ToolCalls: []chat.ToolCall{{ID: "call_9", Type: "function", Function: chat.ToolCallFunction{Name: "read_file", Arguments: `{"path":"x"}`}}}},
		chat.Message{Role: "tool", ToolCallID: "call_9", Content: session.CancelledToolResult})

	captureOutput(t, func() { r.StartCompaction(context.Background(), conv).Finish() })

	if len(conv.Messages) != 2 || conv.Messages[1].CompactionBlock() == nil || !strings.Contains(conv.Messages[1].CompactionBlock().Data, "ENC") {
		t.Fatalf("conversation after compaction = %+v, want the system prompt and the compacted window", conv.Messages)
	}
	if req := p.Requests()[0]; !strings.Contains(req, "call_9") || !strings.Contains(req, session.CancelledToolResult) {
		t.Errorf("the compaction request should carry the halted round:\n%s", req)
	}
}
