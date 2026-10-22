package chat

import (
	"strings"
	"testing"
	"time"
)

func TestConvertMessagesToAnthropic(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "System 1"},
		{Role: "system", Content: "System 2"},
		{Role: "user", Content: "User query 1"},
		{Role: "user", Content: "User query 2"}, // Consecutive user block
		{Role: "assistant", Content: "Response 1"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: ToolCallFunction{
						Name:      "test_tool",
						Arguments: `{"key": "val"}`,
					},
				},
				{
					ID:   "call_2",
					Type: "function",
					Function: ToolCallFunction{
						Name:      "bad_tool",
						Arguments: `invalid json`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_1",
			Content:    "Tool result 1",
		},
	}

	sys, anthropicMsgs := convertMessagesToAnthropic(msgs)
	if sys != "System 1\n\nSystem 2" {
		t.Fatalf("unexpected system string: %q", sys)
	}

	if len(anthropicMsgs) == 0 {
		t.Fatal("expected anthropic messages")
	}

	// Verify tools conversion
	tDefs := []ToolDefinition{
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "fn1",
				Description: "fn1 desc",
				Parameters:  nil,
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "fn2",
				Description: "fn2 desc",
				Parameters: map[string]any{
					"type": "object",
				},
			},
		},
	}
	tools := convertToolsToAnthropic(tDefs)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if convertToolsToAnthropic(nil) != nil {
		t.Fatal("expected nil for empty tools")
	}

	// Malformed tool arguments must be preserved verbatim instead of being
	// silently replaced with an empty object.
	var sawMalformed bool
	for _, m := range anthropicMsgs {
		for _, block := range m.Content {
			if block.Type != "tool_use" || block.Name != "bad_tool" {
				continue
			}
			input, ok := block.Input.(map[string]any)
			if !ok {
				t.Fatalf("tool_use input must be an object, got %T", block.Input)
			}
			raw, ok := input["_malformed_arguments"]
			if !ok || raw != "invalid json" {
				t.Fatalf("expected malformed arguments preserved, got %+v", input)
			}
			sawMalformed = true
		}
	}
	if !sawMalformed {
		t.Fatal("expected bad_tool tool_use block to be present")
	}
}

// TestAnthropicAccumulatorSpeed checks that the speed reported in message_start usage is kept,
// that message_delta usage overrides it, and that a stream reporting none leaves it empty.
func TestAnthropicAccumulatorSpeed(t *testing.T) {
	speedOf := func(events ...[2]string) string {
		t.Helper()
		acc := newAnthropicAccumulator(time.Now())
		for _, ev := range events {
			if err := acc.handle(ev[0], ev[1], nil, nil); err != nil {
				t.Fatalf("handle(%s): %v", ev[0], err)
			}
		}
		_, usage := acc.result()
		return usage.Speed
	}
	start := func(usage string) [2]string {
		return [2]string{"message_start", `{"type":"message_start","message":{"usage":` + usage + `}}`}
	}
	delta := func(usage string) [2]string {
		return [2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + usage + `}`}
	}

	if got := speedOf(start(`{"input_tokens":1,"speed":"fast"}`), delta(`{"output_tokens":2}`)); got != "fast" {
		t.Errorf("speed from message_start = %q, want fast", got)
	}
	if got := speedOf(start(`{"input_tokens":1,"speed":"fast"}`), delta(`{"output_tokens":2,"speed":"standard"}`)); got != "standard" {
		t.Errorf("speed from message_delta = %q, want standard", got)
	}
	if got := speedOf(start(`{"input_tokens":1}`), delta(`{"output_tokens":2}`)); got != "" {
		t.Errorf("unreported speed = %q, want empty", got)
	}
}

func TestAnthropicAccumulator(t *testing.T) {
	acc := newAnthropicAccumulator(time.Now())

	mustHandle := func(eventType, data string, onDelta func(string)) {
		t.Helper()
		if err := acc.handle(eventType, data, nil, onDelta); err != nil {
			t.Fatalf("handle(%s) failed: %v", eventType, err)
		}
	}

	// 1. message_start
	mustHandle("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":2}}}`, nil)

	// 2. content_block_start (text)
	mustHandle("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`, nil)

	// 3. content_block_delta (text_delta)
	var streamedText strings.Builder
	mustHandle("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello world"}}`, func(token string) {
		streamedText.WriteString(token)
	})

	// 4. content_block_start (tool_use)
	mustHandle("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_anthro","name":"run_command"}}`, nil)

	// 5. content_block_delta (input_json_delta)
	mustHandle("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\": \"ls\"}"}}`, nil)

	// 6. message_delta
	mustHandle("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`, nil)

	// 7. message_stop
	mustHandle("message_stop", `{"type":"message_stop"}`, nil)

	msg, usage := acc.result()
	if msg.Content != "hello world" {
		t.Fatalf("expected 'hello world', got %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Function.Name != "run_command" {
		t.Fatalf("expected run_command, got %s", msg.ToolCalls[0].Function.Name)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 20 || usage.CachedTokens != 2 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}
