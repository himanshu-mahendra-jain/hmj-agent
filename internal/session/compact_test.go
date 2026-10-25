package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
)

// chatTurns builds n plain question/answer turns after a system prompt.
func chatTurns(n int) *Conversation {
	conv := &Conversation{ID: "c", Name: "c", Messages: []chat.Message{{Role: "system", Content: "SYSTEM"}}}
	for i := 1; i <= n; i++ {
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
			chat.Message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)},
		)
	}
	return conv
}

func summaryMessage(text string) chat.Message {
	return chat.Message{Role: "assistant", Blocks: []chat.Block{{Type: chat.BlockSummary, Text: text}}}
}

// TestNeedsCompactionOnlyOverTheLimit checks that the size alone decides: many turns under the
// limit are not compacted, and a conversation over it is, whatever its turn count.
func TestNeedsCompactionOnlyOverTheLimit(t *testing.T) {
	if conv := chatTurns(50); conv.NeedsCompaction() {
		t.Error("50 short turns need compaction; only the size should count")
	}

	conv := chatTurns(1)
	conv.SetPromptTokens(CompactAtTokens)
	if conv.NeedsCompaction() {
		t.Error("a conversation of exactly the limit needs compaction; only one over it should")
	}
	conv.Messages = append(conv.Messages, chat.Message{Role: "assistant", Content: "1234"})
	if !conv.NeedsCompaction() {
		t.Errorf("a conversation of ~%d tokens does not need compaction", conv.EstimateTokens())
	}

	big := &Conversation{Messages: []chat.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: strings.Repeat("x", 4*CompactAtTokens+8)},
	}}
	if !big.NeedsCompaction() {
		t.Error("a conversation estimated over the limit without a reported size does not need compaction")
	}
}

// TestNeedsCompactionNotForACompactionAlone checks that a conversation holding only its system
// prompt and a compaction is not compacted again, however large its reported size.
func TestNeedsCompactionNotForACompactionAlone(t *testing.T) {
	conv := &Conversation{Messages: []chat.Message{{Role: "system", Content: "s"}, summaryMessage("summary")}}
	conv.SetPromptTokens(CompactAtTokens * 2)
	if conv.NeedsCompaction() {
		t.Error("a lone compaction needs compaction")
	}
	conv.Messages = append(conv.Messages, chat.Message{Role: "user", Content: "next"})
	if !conv.NeedsCompaction() {
		t.Error("a compaction followed by a turn over the limit does not need compaction")
	}
}

// TestApplyCompactionReplacesTheCompactedMessages checks that the compaction takes the place of
// the messages it was made from, right after the system prompt, that messages appended since are
// kept, that the reported size is forgotten, and that the conversation is marked as changed.
func TestApplyCompactionReplacesTheCompactedMessages(t *testing.T) {
	conv := chatTurns(3)
	upTo := len(conv.Messages)
	conv.Messages = append(conv.Messages, chat.Message{Role: "user", Content: "newer"})
	conv.SetPromptTokens(500_000)

	if err := conv.ApplyCompaction(summaryMessage("the summary"), upTo); err != nil {
		t.Fatal(err)
	}
	if len(conv.Messages) != 3 || conv.Messages[0].Content != "SYSTEM" ||
		conv.Messages[1].CompactionBlock() == nil || conv.Messages[2].Content != "newer" {
		t.Fatalf("messages after compaction = %+v, want the system prompt, the compaction, then the newer message", conv.Messages)
	}
	if conv.LastPromptTokens != 0 || conv.TokenMarkIndex != 0 {
		t.Errorf("reported size kept: %d at %d", conv.LastPromptTokens, conv.TokenMarkIndex)
	}
	if conv.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not set by the compaction")
	}
	if got, want := conv.EstimateTokens(), (len("SYSTEM")+len("the summary")+len("newer"))/4; got != want {
		t.Errorf("estimate after compaction = %d, want %d (the summary counted)", got, want)
	}
}

// TestApplyCompactionRejectsAShorterConversation checks that a compaction made from more messages
// than the conversation now has is not applied.
func TestApplyCompactionRejectsAShorterConversation(t *testing.T) {
	conv := chatTurns(2)
	before := len(conv.Messages)
	if err := conv.ApplyCompaction(summaryMessage("s"), before+1); !errors.Is(err, ErrCompactionOutdated) {
		t.Fatalf("ApplyCompaction = %v, want ErrCompactionOutdated", err)
	}
	if len(conv.Messages) != before {
		t.Error("the conversation changed")
	}
}
