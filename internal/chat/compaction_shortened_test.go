package chat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"hmj-agent/v2/internal/config"
)

// TestShortenedTranscriptCutsEachToolField checks that a shortened transcript cuts each string of
// a tool call's arguments on its own, so the path of a large write survives, cuts tool results,
// keeps an earlier compaction's summary, and leaves out reasoning blocks and their signatures.
func TestShortenedTranscriptCutsEachToolField(t *testing.T) {
	content := strings.Repeat("த", 20_000) // 60 KB of Tamil, cut inside a character
	messages := []Message{
		{Role: "assistant", Blocks: []Block{{Type: BlockSummary, Text: "EARLIER SUMMARY"}}},
		{Role: "user", Content: "translate the strings"},
		{Role: "assistant", Content: "Tamil:", Blocks: []Block{{Type: "thinking", Text: "THOUGHT", Signature: "SIG"}},
			ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "write_file", Arguments: fmt.Sprintf(`{"content":%q,"path":"tr/ta.txt"}`, content)}}}},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("r", 5000)},
	}
	got := shortenedTranscript(messages, shortenedBudget)

	for _, want := range []string{
		"[summary of the earlier conversation]\nEARLIER SUMMARY",
		"[user]\ntranslate the strings",
		"[assistant]\nTamil:\n[called write_file]\n",
		`"path":"tr/ta.txt"`,
		"[… 58 KB left out]",
		"[write_file result]\n",
		"[… 4 KB left out]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript lacks %q:\n%.2000s", want, got)
		}
	}
	for _, unwanted := range []string{"THOUGHT", "SIG", "\uFFFD"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("transcript contains %q", unwanted)
		}
	}
	if len(got) > 4*maxTranscriptToolBytes {
		t.Errorf("transcript is %d bytes, want the tool call and result cut", len(got))
	}
}

// TestShortenedTranscriptLeavesOutTheOldest checks that over the budget the first request is
// kept, then a note, then the newest messages that fit.
func TestShortenedTranscriptLeavesOutTheOldest(t *testing.T) {
	messages := []Message{{Role: "user", Content: "FIRST REQUEST"}}
	for i := range 100 {
		messages = append(messages, Message{Role: "assistant", Content: fmt.Sprintf("step %03d %s", i, strings.Repeat("x", 990))})
	}
	got := shortenedTranscript(messages, 20_000)

	if !strings.HasPrefix(got, "[user]\nFIRST REQUEST\n\n[") {
		t.Errorf("the first request is not kept first:\n%.200s", got)
	}
	if !strings.Contains(got, "earlier messages left out]") || !strings.Contains(got, "step 099") || strings.Contains(got, "step 000") {
		t.Errorf("want the oldest steps left out and the newest kept:\n%.400s", got)
	}
	if len(got) > 20_000+100 {
		t.Errorf("transcript is %d bytes, over the 20000-byte budget", len(got))
	}
	if all := shortenedTranscript(messages, shortenedBudget); strings.Contains(all, "left out]") {
		t.Error("a transcript within the budget left messages out")
	}
}

// TestShortenArgumentsThatAreNotJSON checks that arguments that do not parse are cut as text.
func TestShortenArgumentsThatAreNotJSON(t *testing.T) {
	got := shortenArguments("{" + strings.Repeat("a", 3000))
	if !strings.HasPrefix(got, "{aaa") || !strings.HasSuffix(got, "[… 2 KB left out]") {
		t.Errorf("shortened arguments = %.80q…", got)
	}
}

// overflowServer answers every request over limit bytes as too long, in each protocol's error
// shape, and streams the text "SHORT SUMMARY" otherwise. It records the request paths and bodies.
type overflowServer struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func newOverflowServer(t *testing.T, api string, limit int) *overflowServer {
	t.Helper()
	tooLong := map[string]string{
		config.APIAnthropic:   `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1020897 tokens > 1000000 maximum"}}`,
		config.APIResponses:   `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","code":"context_length_exceeded"}}`,
		config.APICompletions: `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`,
		config.APIGemini:      `{"error":{"code":400,"message":"The input token count exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`,
	}[api]
	stream := map[string]string{
		config.APIAnthropic: "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"SHORT SUMMARY\"}}\n\n" + anthropicEndTurn,
		config.APIResponses: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"SHORT SUMMARY\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
		config.APICompletions: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"SHORT SUMMARY\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		config.APIGemini: "data: {\"event_type\":\"step.delta\",\"index\":0,\"delta\":{\"type\":\"text\",\"text\":\"SHORT SUMMARY\"}}\n\n" +
			"data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"status\":\"completed\"}}\n\n",
	}[api]
	s := &overflowServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.bodies = append(s.bodies, string(body))
		s.mu.Unlock()
		if len(body) > limit {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, tooLong)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	t.Cleanup(s.Close)
	return s
}

// overflowTestClient returns a client of the given protocol for srv, without retries.
func overflowTestClient(api string, srv *overflowServer) *Client {
	base := srv.URL
	if api == config.APIGemini {
		base += "/v1beta"
	}
	return clientFor(api, base, 0)
}

// tooLongHistory is a conversation whose one large write makes any request that sends it whole
// over 100 KB, with a signed thinking block before the write.
var tooLongHistory = []Message{
	{Role: "system", Content: "SYSTEM"},
	{Role: "user", Content: "translate the strings"},
	{Role: "assistant", Content: "Tamil:", Blocks: []Block{{Type: "thinking", Text: "THOUGHT", Signature: "THINKING-SIG"}},
		ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "write_file", Arguments: fmt.Sprintf(`{"content":%q,"path":"tr/ta.txt"}`, strings.Repeat("த", 40_000))}}}},
	{Role: "tool", ToolCallID: "c1", Content: "successfully wrote 120000 bytes"},
	{Role: "assistant", Content: "[error: prompt is too long]"},
}

// TestCompactionTooLongSummarizesAShortenedCopy checks, for every protocol, that a compaction the
// provider finds too long is followed straight by one shortened summary request, a plain-text
// transcript after the system prompt with no tools, tool calls or signed reasoning, and that its
// summary becomes the compaction.
func TestCompactionTooLongSummarizesAShortenedCopy(t *testing.T) {
	for _, api := range allAPIs {
		t.Run(api, func(t *testing.T) {
			srv := newOverflowServer(t, api, 100_000)
			defs := []ToolDefinition{{Type: "function", Function: FunctionDefinition{Name: "write_file", Description: "writes a file"}}}
			compacted, err := overflowTestClient(api, srv).Compact(context.Background(), testRef, tooLongHistory, defs)
			if err != nil {
				t.Fatal(err)
			}
			if b := compacted.CompactionBlock(); b == nil || b.Type != BlockSummary || b.Text != "SHORT SUMMARY" {
				t.Errorf("compaction message = %+v, want the summary of the shortened copy", compacted)
			}
			if len(srv.bodies) != 2 {
				t.Fatalf("%d requests to %v, want the too-long one and then the shortened summary", len(srv.bodies), srv.paths)
			}
			short := srv.bodies[1]
			if !strings.Contains(short, "shortened transcript") || !strings.Contains(short, "Write a summary") || !strings.Contains(short, "SYSTEM") {
				t.Errorf("the second request is not the shortened summary request:\n%.1000s", short)
			}
			for _, unwanted := range []string{"THINKING-SIG", "THOUGHT", "write_file\"", "writes a file", `"compaction"`} {
				if strings.Contains(short, unwanted) {
					t.Errorf("the shortened request carries %q:\n%.1000s", unwanted, short)
				}
			}
		})
	}
}

// TestCompactionTooLongHalvesTheShortenedCopy checks that a shortened transcript the provider
// still finds too long is halved each time, and that after shortenedAttempts the compaction fails
// with the provider's refusal.
func TestCompactionTooLongHalvesTheShortenedCopy(t *testing.T) {
	history := []Message{{Role: "system", Content: "SYSTEM"}, {Role: "user", Content: "go"}}
	for i := range 2000 {
		history = append(history, Message{Role: "assistant", Content: fmt.Sprintf("step %d %s", i, strings.Repeat("x", 1000))})
	}
	srv := newOverflowServer(t, config.APIAnthropic, 1000)
	_, err := overflowTestClient(config.APIAnthropic, srv).Compact(context.Background(), testRef, history, nil)
	if !IsContextOverflow(err) {
		t.Fatalf("err = %v, want the provider's refusal", err)
	}
	if len(srv.bodies) != 1+shortenedAttempts {
		t.Fatalf("%d requests, want the compaction request and %d shortened ones", len(srv.bodies), shortenedAttempts)
	}
	for i := 2; i < len(srv.bodies); i++ {
		prev, cur := len(srv.bodies[i-1]), len(srv.bodies[i])
		if cur > prev/2+10_000 || cur < prev/2-10_000 {
			t.Errorf("shortened request %d is %d bytes after %d, want about half", i, cur, prev)
		}
	}
}
