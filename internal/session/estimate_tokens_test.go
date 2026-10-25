package session

import (
	"testing"

	"hmj-agent/v2/internal/chat"
)

// TestEstimateTokensUsesReportedPromptSize checks that the reported prompt size is the baseline and
// that only messages appended after it are estimated, and that rewriting the transcript forgets it.
func TestEstimateTokensUsesReportedPromptSize(t *testing.T) {
	conv := &Conversation{Messages: []chat.Message{{Role: "user", Content: "hello world"}}}
	if got := conv.EstimateTokens(); got != 2 {
		t.Fatalf("no baseline: estimate = %d, want 2", got)
	}

	conv.SetPromptTokens(1000)
	conv.Messages = append(conv.Messages, chat.Message{Role: "assistant", Content: "12345678"})
	if got := conv.EstimateTokens(); got != 1002 {
		t.Errorf("with baseline: estimate = %d, want 1000 + 8/4", got)
	}

	// Compaction rewrites the transcript, so the reported size no longer applies.
	if err := conv.ApplyCompaction(summaryMessage("summary"), len(conv.Messages)); err != nil {
		t.Fatal(err)
	}
	if conv.LastPromptTokens != 0 || conv.TokenMarkIndex != 0 {
		t.Errorf("after rewrite: LastPromptTokens=%d TokenMarkIndex=%d, want both 0", conv.LastPromptTokens, conv.TokenMarkIndex)
	}
}
