package chat

import (
	"testing"
	"time"
)

// TestToolOnlyStepIsTimed checks that a response made only of a tool call gets a time to first
// output and an output rate, as a text reply does, while the tool call alone still counts as no
// shown output, so a failure before any text can be retried.
func TestToolOnlyStepIsTimed(t *testing.T) {
	// The request started a second ago, so the first output is about a second in.
	start := time.Now().Add(-time.Second)

	t.Run("anthropic", func(t *testing.T) {
		acc := newAnthropicAccumulator(start)
		for _, ev := range [][2]string{
			{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"list_files"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`},
		} {
			if err := acc.handle(ev[0], ev[1], nil, nil); err != nil {
				t.Fatalf("handle(%s): %v", ev[0], err)
			}
		}
		if !acc.firstToken.IsZero() {
			t.Error("a tool call counted as shown output, so a failure could no longer be retried")
		}
		_, usage := acc.result()
		assertTimed(t, usage)
	})

	t.Run("chat completions", func(t *testing.T) {
		acc := newCompletionsAccumulator(start)
		for _, chunk := range []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"list_files","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`,
		} {
			if err := acc.handle(chunk, nil, nil); err != nil {
				t.Fatalf("handle: %v", err)
			}
		}
		if !acc.firstToken.IsZero() {
			t.Error("a tool call counted as shown output, so a failure could no longer be retried")
		}
		_, usage := acc.result()
		assertTimed(t, usage)
	})
}

func assertTimed(t *testing.T, usage *TokenUsage) {
	t.Helper()
	if usage.TTFT < time.Second || usage.TTFT >= usage.Duration {
		t.Errorf("TTFT = %v of %v, want about a second and less than the whole response", usage.TTFT, usage.Duration)
	}
	if usage.TokensPerSecond <= 0 {
		t.Errorf("TokensPerSecond = %v, want a rate", usage.TokensPerSecond)
	}
}
