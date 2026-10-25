package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/tools"
)

// Regression tests for a session that stopped for good: a fetch_url of googlesource's
// Notification.java?format=TEXT returned 823 KB of base64 on one line, the next request was
// 1.2M tokens, Anthropic answered "prompt is too long", and the saved conversation failed the
// same way on every later request.

// anthropicStream answers with the given Anthropic stream events.
func anthropicStream(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, data := range events {
			kind := data[strings.Index(data, `"type":"`)+8:]
			kind = kind[:strings.IndexByte(kind, '"')]
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		}
	}
}

// anthropicToolCall answers with one call to the named tool.
func anthropicToolCall(name, args string) http.HandlerFunc {
	return anthropicStream(
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":%q}}`, name),
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, args),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	)
}

// anthropicText answers with text.
func anthropicText(text string) http.HandlerFunc {
	return anthropicStream(
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	)
}

// The Anthropic API's answer to the incident's request, as it came.
var anthropicPromptTooLong = statusReply(http.StatusBadRequest,
	`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1200054 tokens > 1000000 maximum"},"request_id":"req_011CgHhUsPCENWa6Z3FtNXNf"}`)

// runFetchTurn runs a turn on the Anthropic protocol in which the model fetches pageURL with
// the real fetch_url tool, and returns the provider's request bodies.
func runFetchTurn(t *testing.T, pageURL string, replies ...http.HandlerFunc) ([]string, *session.Conversation) {
	t.Helper()
	t.Chdir(t.TempDir())
	tools.SetFetchDir(t.TempDir())
	t.Cleanup(func() { tools.SetFetchDir("") })
	p := newScriptedProvider(t, append([]http.HandlerFunc{anthropicToolCall("fetch_url", `{"url":"`+pageURL+`"}`)}, replies...)...)
	cfg := p.config("anthropic")
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{&tools.FetchURLTool{}})
	conv := &session.Conversation{ID: "1", Model: mockModel}
	captureOutput(t, func() {
		if err := r.Run(context.Background(), conv, "why is the notification text cut at 1024?"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return p.Requests(), conv
}

// One 800 KB line from fetch_url, base64 or not, adds at most about MaxFetchOutputBytes to the
// next request.
func TestRegressionOneHugeFetchedLineStaysSmall(t *testing.T) {
	java := strings.Repeat("    static final int MAX_CHARSEQUENCE_LENGTH = 1024; // limit\n", 14000) // ~800 KB
	bodies := map[string]string{
		"base64 source":        base64.StdEncoding.EncodeToString([]byte(java)),
		"one line, not base64": strings.Repeat("var a=1;", 100*1024),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Write([]byte(body))
			}))
			defer page.Close()

			reqs, _ := runFetchTurn(t, page.URL+"/Notification.java?format=TEXT", anthropicText("done"))
			if len(reqs) != 2 {
				t.Fatalf("%d requests, want 2", len(reqs))
			}
			if grew := len(reqs[1]) - len(reqs[0]); grew > tools.MaxFetchOutputBytes+16*1024 {
				t.Errorf("the fetch added %d bytes to the request; want at most about %d", grew, tools.MaxFetchOutputBytes)
			}
			if !strings.Contains(reqs[1], "the full text is saved to ") {
				t.Error("the result does not name the saved full text")
			}
		})
	}
}

// When Anthropic still finds the prompt too long, the large result is removed, the turn goes on,
// and the conversation keeps the removal, so it can be sent again.
func TestRegressionAnthropicPromptTooLongIsRecovered(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, anthropicToolCall("big", "{}"), anthropicPromptTooLong, anthropicText("done"))
	cfg := p.config("anthropic")
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{bigTool{size: 200 * 1024}})
	conv := &session.Conversation{ID: "1", Model: mockModel}
	captureOutput(t, func() {
		if err := r.Run(context.Background(), conv, "go on"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	if got := toolResult(t, conv); !strings.HasPrefix(got, "[the big result (200 KB) was removed") {
		t.Errorf("tool result is %q", got[:min(len(got), 120)])
	}
	reqs := p.Requests()
	if len(reqs) != 3 || strings.Contains(reqs[2], "payload payload") {
		t.Errorf("want 3 requests, the last without the result; got %d", len(reqs))
	}
	if last := conv.Messages[len(conv.Messages)-1]; last.Content != "done" {
		t.Errorf("turn ended with %+v", last)
	}
}
