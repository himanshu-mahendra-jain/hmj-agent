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

// A compatible server's error chunk mid-stream is reported, not taken for an empty reply.
func TestCompletionsStreamErrorChunk(t *testing.T) {
	acc := newCompletionsAccumulator(time.Now())
	if err := acc.handle(`{"error":{"message":"Out of memory.","code":500}}`, nil, nil); err == nil || err.Error() != "chat completions stream error: Out of memory." {
		t.Errorf("err = %v", err)
	}
	if err := acc.handle(`{"error":{}}`, nil, nil); err == nil || !strings.Contains(err.Error(), `{"error":{}}`) {
		t.Errorf("err without a message = %v, want the chunk shown", err)
	}
}

// OpenAI's own API and Azure OpenAI get max_completion_tokens; other servers max_tokens.
func TestIsOpenAIHost(t *testing.T) {
	for base, want := range map[string]bool{
		"https://api.openai.com/v1":                   true,
		"https://API.OpenAI.com/v1/chat/completions":  true,
		"https://myres.openai.azure.com/openai/v1":    true,
		"http://localhost:11434/v1":                   false,
		"https://openrouter.ai/api/v1":                false,
		"https://api.openai.com.evil.example/v1":      false,
		"https://generativelanguage.googleapis.com/x": false,
	} {
		if got := isOpenAIHost(base); got != want {
			t.Errorf("isOpenAIHost(%q) = %v, want %v", base, got, want)
		}
	}
}
