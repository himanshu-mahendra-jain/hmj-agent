package session

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"hmj-agent/v2/internal/chat"
)

// CompactAtTokens is the context size, in tokens, above which a conversation is compacted once
// its turn has ended. A turn is never compacted while it runs, even past this size.
const CompactAtTokens = 400_000

// ErrCompactionOutdated is returned by ApplyCompaction when the conversation no longer starts
// with the messages the compaction was made from.
var ErrCompactionOutdated = errors.New("the conversation changed while it was being compacted")

// EstimateTokens approximates the size of the conversation in tokens. When the provider has
// reported the prompt size of an earlier request (SetPromptTokens), that measured size is the
// baseline and only the messages appended since are estimated, at four bytes per token, which is
// close for English prose and somewhat low for code.
func (c *Conversation) EstimateTokens() int {
	if c.LastPromptTokens > 0 && c.TokenMarkIndex >= 0 && c.TokenMarkIndex <= len(c.Messages) {
		return c.LastPromptTokens + messageBytes(c.Messages[c.TokenMarkIndex:])/4
	}
	return messageBytes(c.Messages) / 4
}

// messageBytes counts the content, tool-call and compaction bytes of msgs.
func messageBytes(msgs []chat.Message) int {
	n := 0
	for i := range msgs {
		m := &msgs[i]
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
		if b := m.CompactionBlock(); b != nil {
			n += len(b.Text) + len(b.Data)
		}
	}
	return n
}

// NeedsCompaction reports whether the conversation is over CompactAtTokens and has something to
// compact: messages after its system prompt other than a compaction alone.
func (c *Conversation) NeedsCompaction() bool {
	if c.EstimateTokens() <= CompactAtTokens {
		return false
	}
	rest := c.Messages[c.historyStart():]
	return len(rest) > 1 || (len(rest) == 1 && rest[0].CompactionBlock() == nil)
}

// IsCompacted reports whether the conversation has been compacted: its history starts with a
// compaction instead of its first turn.
func (c *Conversation) IsCompacted() bool {
	start := c.historyStart()
	return start < len(c.Messages) && c.Messages[start].CompactionBlock() != nil
}

// historyStart returns the index of the first message after the system prompt.
func (c *Conversation) historyStart() int {
	if len(c.Messages) > 0 && c.Messages[0].Role == "system" {
		return 1
	}
	return 0
}

// ApplyCompaction replaces the first upTo messages, the ones the compaction was made from, with
// compacted, keeping the system prompt and any message appended since. The replaced messages are
// archived first for /history. It returns ErrCompactionOutdated, and changes nothing, when the
// conversation has fewer messages than that. When archiving fails the compaction is applied all
// the same, and the error returned wraps ErrHistoryNotSaved.
func (c *Conversation) ApplyCompaction(compacted chat.Message, upTo int) error {
	start := c.historyStart()
	if upTo < start || upTo > len(c.Messages) {
		return ErrCompactionOutdated
	}
	archiveErr := c.archive(c.Messages[start:upTo])
	c.Messages = slices.Concat(c.Messages[:start], []chat.Message{compacted}, c.Messages[upTo:])
	c.resetTokenMark()
	c.UpdatedAt = time.Now()
	if archiveErr != nil {
		return fmt.Errorf("%w: %w", ErrHistoryNotSaved, archiveErr)
	}
	return nil
}
