package chat

import (
	"strings"
	"testing"
)

// TestPortableCompactionsSendsTheSummaryAsText checks that a compaction a protocol cannot send as
// it is reaches the model as a user message with the summary, then the acknowledgement, and that
// one only another provider can read is replaced by a notice.
func TestPortableCompactionsSendsTheSummaryAsText(t *testing.T) {
	history := []Message{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Blocks: []Block{{Type: BlockSummary, Text: "the summary"}}},
		{Role: "user", Content: "next"},
	}
	got := portableCompactions(history, BlockAnthropicCompaction)
	if len(got) != 4 || got[1].Role != "user" || got[1].Content != SummaryPrefix+"\nthe summary" ||
		got[2].Role != "assistant" || got[2].Content != SummaryAck || got[3].Content != "next" {
		t.Fatalf("portable history = %+v", got)
	}
	if history[1].CompactionBlock() == nil {
		t.Error("the conversation's own messages were changed")
	}

	encrypted := []Message{{Role: "assistant", Blocks: []Block{{Type: BlockResponsesCompaction, Data: `[{"type":"compaction"}]`}}}}
	if got := portableCompactions(encrypted, ""); !strings.Contains(got[0].Content, unreadableSummary) {
		t.Errorf("an unreadable compaction became %q", got[0].Content)
	}
	if got := portableCompactions(encrypted, BlockResponsesCompaction); len(got) != 1 || got[0].CompactionBlock() == nil {
		t.Error("a compaction the protocol can send was replaced")
	}
}

// TestEveryProtocolSendsAForeignCompactionAsText checks that each protocol's converter sends a
// summary compaction as plain text.
func TestEveryProtocolSendsAForeignCompactionAsText(t *testing.T) {
	history := []Message{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Blocks: []Block{{Type: BlockSummary, Text: "the summary"}}},
		{Role: "user", Content: "next"},
	}
	_, anthropic := convertMessagesToAnthropic(history)
	if len(anthropic) != 3 || anthropic[0].Role != "user" || !strings.Contains(anthropic[0].Content[0].Text, "the summary") {
		t.Errorf("Anthropic messages = %+v", anthropic)
	}
	if items := convertMessagesToInputItems(history); len(items) != 4 || !strings.Contains(items[1].Content, "the summary") {
		t.Errorf("Responses input = %+v", items)
	}
	if _, steps := convertMessagesToGemini(history); len(steps) != 3 || !strings.Contains(steps[0].Content[0].Text, "the summary") {
		t.Errorf("Gemini input = %+v", steps)
	}
	if msgs := convertMessagesToCompletions(history); len(msgs) != 4 || !strings.Contains(msgs[1].Content, "the summary") {
		t.Errorf("Completions messages = %+v", msgs)
	}
}
