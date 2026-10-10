package chat

import (
	"strings"
	"testing"
	"time"
)

func TestConvertMessagesToCompletions(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{
			Role:    "assistant",
			Content: "calling tool",
			ToolCalls: []ToolCall{
				{
					ID: "tc1",
					Function: ToolCallFunction{
						Name:      "f",
						Arguments: "{}",
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "tc1",
			Content:    "result",
		},
	}
	compMsgs := convertMessagesToCompletions(msgs)
	if len(compMsgs) != 3 {
		t.Fatalf("expected 3 completions messages, got %d", len(compMsgs))
	}
	if compMsgs[0].Role != "user" || compMsgs[1].Role != "assistant" || compMsgs[2].Role != "tool" {
		t.Fatalf("unexpected roles: %+v", compMsgs)
	}
}

func TestCompletionsAccumulator(t *testing.T) {
	acc := newCompletionsAccumulator(time.Now())

	mustChunk := func(data string, onDelta func(string)) {
		t.Helper()
		if err := acc.handle(data, nil, onDelta); err != nil {
			t.Fatalf("handle failed: %v", err)
		}
	}

	mustChunk(`{"choices":[{"index":0,"delta":{"content":"chunk 1 "}}]}`, nil)

	var streamed strings.Builder
	mustChunk(`{"choices":[{"index":0,"delta":{"content":"chunk 2","tool_calls":[{"index":0,"id":"tc_comp_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`, func(token string) {
		streamed.WriteString(token)
	})

	// Subsequent chunk finishing tool call
	mustChunk(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" \"test.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":15,"completion_tokens":25,"total_tokens":40}}`, nil)

	msg, usage := acc.result()
	if msg.Content != "chunk 1 chunk 2" {
		t.Fatalf("expected 'chunk 1 chunk 2', got %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Function.Arguments != `{"path": "test.txt"}` {
		t.Fatalf("arguments mismatch: %q", msg.ToolCalls[0].Function.Arguments)
	}
	if usage == nil || usage.PromptTokens != 15 || usage.CompletionTokens != 25 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}
