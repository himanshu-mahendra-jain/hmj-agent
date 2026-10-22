package chat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hmj-agent/internal/config"
)

// recordedThinkingStream is an Anthropic stream with a signed thinking block, a redacted thinking
// block and a tool call, as the Messages API emits it for a thinking-enabled model.
const recordedThinkingStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":50,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me read it."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG-ABC"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"ENC-XYZ"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicEndTurn ends an Anthropic test stream as the API does, with the stop reason and
// message_stop. A stream without a stop reason is reported as incomplete.
const anthropicEndTurn = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// anthropicTestClient returns a client whose provider is the test server at url, with retries
// that wait only a millisecond.
func anthropicTestClient(url string, retries int) *Client {
	c := NewClient(config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: url, APIKey: "k", API: "anthropic", Models: []config.Model{{ID: "m"}}},
	}})
	c.RetryConfig = RetryConfig{MaxRetries: retries, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, BackoffFactor: 1}
	return c
}

var testRef = config.ModelRef{Provider: "p", Model: "m"}

// TestAnthropicThinkingBlocksRoundTrip checks that a streamed thinking block with its signature is
// kept, and that the next request replays it verbatim before the tool_use block.
func TestAnthropicThinkingBlocksRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, recordedThinkingStream)
	}))
	defer server.Close()

	client := anthropicTestClient(server.URL, 0)
	msg, usage, err := client.StreamChat(context.Background(), testRef, []Message{{Role: "user", Content: "read a.go"}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if usage.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", usage.FinishReason)
	}
	wantBlocks := []Block{
		{Type: "thinking", Text: "Let me read it.", Signature: "SIG-ABC"},
		{Type: "redacted_thinking", Data: "ENC-XYZ"},
	}
	if len(msg.Blocks) != len(wantBlocks) {
		t.Fatalf("blocks = %+v, want %+v", msg.Blocks, wantBlocks)
	}
	for i := range wantBlocks {
		if msg.Blocks[i] != wantBlocks[i] {
			t.Errorf("block %d = %+v, want %+v", i, msg.Blocks[i], wantBlocks[i])
		}
	}

	// Replay the assistant turn and check the first assistant content block.
	_, replayed := convertMessagesToAnthropic([]Message{
		{Role: "user", Content: "read a.go"},
		msg,
		{Role: "tool", ToolCallID: "toolu_1", Content: "package main"},
	})
	assistant := replayed[1]
	if assistant.Role != "assistant" || len(assistant.Content) < 3 {
		t.Fatalf("assistant message = %+v", assistant)
	}
	first := assistant.Content[0]
	if first.Type != "thinking" || first.Thinking == nil || *first.Thinking != "Let me read it." || first.Signature != "SIG-ABC" {
		t.Errorf("first assistant block = %+v, want the signed thinking block", first)
	}
	if assistant.Content[1].Type != "redacted_thinking" || assistant.Content[1].Data != "ENC-XYZ" {
		t.Errorf("second assistant block = %+v, want the redacted thinking block", assistant.Content[1])
	}
	if last := assistant.Content[len(assistant.Content)-1]; last.Type != "tool_use" {
		t.Errorf("last assistant block = %q, want tool_use", last.Type)
	}

	// The wire payload carries the signature and adaptive thinking with summarized display.
	body, err := json.Marshal(anthropicRequestFor(config.Model{ID: "m"}, []Message{msg}, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"signature":"SIG-ABC"`, `"data":"ENC-XYZ"`, `"thinking":{"type":"adaptive","display":"summarized"}`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("request body missing %s: %s", want, body)
		}
	}
	if strings.Contains(string(body), `"budget_tokens"`) {
		t.Errorf("request must not send budget_tokens: %s", body)
	}
}

// TestAnthropicToolErrorIsMarked checks that a failed tool result is sent with is_error set.
func TestAnthropicToolErrorIsMarked(t *testing.T) {
	_, msgs := convertMessagesToAnthropic([]Message{
		{Role: "tool", ToolCallID: "a", Content: "error: no such file"},
		{Role: "tool", ToolCallID: "b", Content: "ok"},
	})
	if len(msgs) != 1 || len(msgs[0].Content) != 2 {
		t.Fatalf("messages = %+v", msgs)
	}
	if !msgs[0].Content[0].IsError || msgs[0].Content[1].IsError {
		t.Errorf("is_error flags = %v, %v; want true, false", msgs[0].Content[0].IsError, msgs[0].Content[1].IsError)
	}
}

// TestAnthropicThinkingNotCacheMarked checks that the cache breakpoint never lands on a thinking
// block, which the API rejects.
func TestAnthropicThinkingNotCacheMarked(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Blocks: []Block{{Type: "thinking", Text: "t", Signature: "s"}}},
	}
	req := anthropicRequestFor(config.Model{ID: "m"}, msgs, nil)
	last := req.Messages[len(req.Messages)-1].Content
	for _, c := range last {
		if c.Type == "thinking" && c.CacheControl != nil {
			t.Fatalf("thinking block carries cache_control: %+v", c)
		}
	}
}

// TestAnthropicMaxOutputTokensAndEffort checks the configured output limit and effort are sent.
func TestAnthropicMaxOutputTokensAndEffort(t *testing.T) {
	req := anthropicRequestFor(config.Model{ID: "m", MaxOutputTokens: 4096, Effort: "xhigh"}, nil, nil)
	if req.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096", req.MaxTokens)
	}
	if req.OutputConfig == nil || req.OutputConfig.Effort != "xhigh" {
		t.Errorf("output_config = %+v, want effort xhigh", req.OutputConfig)
	}
	def := anthropicRequestFor(config.Model{ID: "m"}, nil, nil)
	if def.MaxTokens != DefaultAnthropicMaxTokens || def.OutputConfig != nil {
		t.Errorf("unset model: max_tokens = %d, output_config = %+v", def.MaxTokens, def.OutputConfig)
	}
}

// TestAnthropicSpeed checks that a model set to fast mode sends speed "fast", and that an unset
// speed sends no speed field.
func TestAnthropicSpeed(t *testing.T) {
	fast, err := json.Marshal(anthropicRequestFor(config.Model{ID: "m", Speed: "fast"}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fast), `"speed":"fast"`) {
		t.Errorf("fast model request = %s, want speed fast", fast)
	}
	def, err := json.Marshal(anthropicRequestFor(config.Model{ID: "m"}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(def), `"speed"`) {
		t.Errorf("unset model request = %s, want no speed", def)
	}
}

// TestAnthropicStrictToolsAndHeaders checks strict tool flags and that only x-api-key authenticates.
func TestAnthropicStrictToolsAndHeaders(t *testing.T) {
	tools := convertToolsToAnthropic([]ToolDefinition{{Type: "function", Function: FunctionDefinition{Name: "t"}}})
	if len(tools) != 1 || !tools[0].Strict {
		t.Fatalf("tools = %+v, want strict", tools)
	}

	var gotKey, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicEndTurn)
	}))
	defer server.Close()
	if _, _, err := anthropicTestClient(server.URL, 0).StreamChat(context.Background(), testRef, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if gotKey != "k" || gotAuth != "" {
		t.Errorf("x-api-key = %q, Authorization = %q; want key only", gotKey, gotAuth)
	}
}

// TestAnthropicOverloadedBeforeOutputIsRetried checks that an overloaded_error event sent before any
// output is retried, and the retry's reply is returned.
func TestAnthropicOverloadedBeforeOutputIsRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
			return
		}
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+anthropicEndTurn)
	}))
	defer server.Close()

	msg, _, err := anthropicTestClient(server.URL, 2).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if msg.Content != "ok" || calls.Load() != 2 {
		t.Errorf("content = %q after %d calls, want ok after 2", msg.Content, calls.Load())
	}
}

// TestAnthropicOverloadedAfterOutputIsNotRetried checks that a stream error after output has been
// delivered is returned rather than retried, since the output already reached the user.
func TestAnthropicOverloadedAfterOutputIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"oops\"}}\n\n")
	}))
	defer server.Close()

	_, _, err := anthropicTestClient(server.URL, 2).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "api_error") {
		t.Fatalf("err = %v, want the api_error", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry once output was delivered)", calls.Load())
	}
}

// TestAnthropicRefusalDetails checks that a refusal's stop reason and category are reported.
func TestAnthropicRefusalDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"type\":\"refusal\",\"category\":\"cyber\",\"explanation\":\"no\"}},\"usage\":{\"output_tokens\":1}}\n\n")
	}))
	defer server.Close()

	_, usage, err := anthropicTestClient(server.URL, 0).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if usage.FinishReason != "refusal" || usage.RefusalCategory != "cyber" {
		t.Errorf("usage = finish %q category %q, want refusal/cyber", usage.FinishReason, usage.RefusalCategory)
	}
}

// TestRetryableStatus529 checks that the overloaded HTTP status is retryable.
func TestRetryableStatus529(t *testing.T) {
	if !IsRetryableStatusCode(529) {
		t.Error("529 must be retryable")
	}
	if IsRetryableStatusCode(400) {
		t.Error("400 must not be retryable")
	}
}

// TestStreamStallIsAborted checks the idle watchdog: a stream that stops after its first output is
// aborted with a "stream stalled" error and, since output was already delivered, not retried.
func TestStreamStallIsAborted(t *testing.T) {
	restore := StreamIdleTimeout
	StreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { StreamIdleTimeout = restore })

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer server.Close()

	start := time.Now()
	_, _, err := anthropicTestClient(server.URL, 2).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if err == nil || !errors.Is(err, ErrStreamStalled) || !strings.Contains(err.Error(), "stream stalled") {
		t.Fatalf("err = %v, want a stream stalled error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("stall took %s to detect, want about the idle timeout", elapsed)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry after output)", calls.Load())
	}
}

// TestStreamStallBeforeOutputIsRetried checks that a stall before any output is retried.
func TestStreamStallBeforeOutputIsRetried(t *testing.T) {
	restore := StreamIdleTimeout
	StreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { StreamIdleTimeout = restore })

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+anthropicEndTurn)
	}))
	defer server.Close()

	msg, _, err := anthropicTestClient(server.URL, 2).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if msg.Content != "ok" || calls.Load() != 2 {
		t.Errorf("content = %q after %d calls, want ok after 2", msg.Content, calls.Load())
	}
}

// TestAnthropicWireOmitsOnlyAbsentFields checks that a no-argument tool_use keeps its input object,
// that an empty thinking text is still sent, and that a successful tool result carries no is_error.
// The json/v2 omitempty tag would drop the first two and send the last one.
func TestAnthropicWireOmitsOnlyAbsentFields(t *testing.T) {
	_, msgs := convertMessagesToAnthropic([]Message{
		{Role: "assistant", Blocks: []Block{{Type: "thinking", Signature: "s"}}, ToolCalls: []ToolCall{{ID: "t1", Function: ToolCallFunction{Name: "list_files"}}}},
		{Role: "tool", ToolCallID: "t1", Content: "ok"},
	})
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"input":{}`, `"thinking":""`} {
		if !strings.Contains(s, want) {
			t.Errorf("wire form missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, "is_error") {
		t.Errorf("successful tool result must not send is_error: %s", s)
	}
}
