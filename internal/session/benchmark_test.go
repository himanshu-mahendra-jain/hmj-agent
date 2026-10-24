package session_test

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/session"
)

func buildBenchmarkConversation(turnCount int, toolOutputLines int) *session.Conversation {
	conv := &session.Conversation{
		ID:   "bench-conversation",
		Name: "bench-session",
	}

	var toolBody strings.Builder
	for i := 1; i <= toolOutputLines; i++ {
		fmt.Fprintf(&toolBody, "output line %d: build succeeded with 0 errors\n", i)
	}
	bulkyOutput := toolBody.String()

	for turn := 1; turn <= turnCount; turn++ {
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("Turn %d: please run check and update module", turn)},
			chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: fmt.Sprintf("call_%d", turn)}}},
			chat.Message{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", turn), Content: bulkyOutput},
			chat.Message{Role: "assistant", Content: fmt.Sprintf("Turn %d complete", turn)},
		)
	}

	return conv
}

// BenchmarkSession_JSONSerialization measures session encoding overhead on long sessions.
func BenchmarkSession_JSONSerialization(b *testing.B) {
	conv := buildBenchmarkConversation(30, 30)

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		data, err := json.Marshal(conv)
		if err != nil || len(data) == 0 {
			b.Fatalf("serialization failed: %v", err)
		}
	}
}

// --- Parsing & Matching Benchmarks ---
