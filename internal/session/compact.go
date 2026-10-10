package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"hmj-agent/internal/chat"
)

const (
	// MaxToolOutputLines defines the line threshold above which old tool output is candidate for pruning.
	MaxToolOutputLines = 25
	// MaxToolOutputChars defines the character threshold above which old tool output is candidate for pruning.
	MaxToolOutputChars = 1000
	// RetainHeadLines is the number of starting lines preserved when pruning tool output.
	RetainHeadLines = 10
	// RetainTailLines is the number of ending lines preserved when pruning tool output.
	RetainTailLines = 5
	// DefaultKeepRecentTurns is how many recent user turns are kept completely unpruned.
	DefaultKeepRecentTurns = 1
	// DefaultKeepRecentToolCalls is how many recent tool outputs within the current turn are kept completely unpruned.
	DefaultKeepRecentToolCalls = 2
	// DefaultMaxTurnsBeforeSummary is the number of user turns above which Condense
	// replaces the older turns with a summary.
	DefaultMaxTurnsBeforeSummary = 6
	// DefaultKeepTurnsAfterSummary is how many recent turns Condense keeps word for word.
	DefaultKeepTurnsAfterSummary = 3

	// SummaryTokenAllowance is the room FitContext leaves for the summary when it picks how
	// many recent turns to keep.
	SummaryTokenAllowance = 2000

	// SummaryPrefix opens the user message that carries a summary of condensed turns.
	SummaryPrefix = "[Summary of the earlier part of this conversation, written by the assistant for its own reference]"
	// summaryAck is the assistant reply that follows a summary, so the transcript keeps
	// alternating roles.
	summaryAck = "Understood. I will continue from this summary."
)

// ErrEmptySummary is returned by Condense when the summarizer produced no text.
var ErrEmptySummary = errors.New("the model returned an empty summary")

// Summarizer writes a summary of history, the leading part of a conversation (its system
// prompt, when it has one, and its older turns), for the model to continue from in place
// of those messages.
type Summarizer func(ctx context.Context, history []chat.Message) (string, error)

// PruneOldToolOutputs scans the conversation messages and prunes bulky tool output from turns
// older than keepRecentTurns.
// It returns the number of tool outputs pruned and the total number of bytes saved.
func (c *Conversation) PruneOldToolOutputs(keepRecentTurns int) (prunedCount int, bytesSaved int) {
	if keepRecentTurns < 1 {
		keepRecentTurns = 1
	}

	// User messages mark turn boundaries
	userIndices := c.userIndices()

	if len(userIndices) <= keepRecentTurns {
		// All turns are recent; keep full fidelity
		return 0, 0
	}

	// Cutoff index: everything before userIndices[len(userIndices)-keepRecentTurns] belongs to older turns
	cutoffIndex := userIndices[len(userIndices)-keepRecentTurns]
	return c.pruneToolMessagesBefore(cutoffIndex)
}

// PruneInFlightToolOutputs prunes bulky tool outputs within the conversation,
// preserving the last keepRecentToolCalls tool outputs in full fidelity. This prevents
// intermediate steps in a long-running prompt/turn from repeatedly inflating prompt tokens.
func (c *Conversation) PruneInFlightToolOutputs(keepRecentToolCalls int) (prunedCount int, bytesSaved int) {
	if keepRecentToolCalls < 1 {
		keepRecentToolCalls = 1
	}

	var toolIndices []int
	for i, m := range c.Messages {
		if m.Role == "tool" {
			toolIndices = append(toolIndices, i)
		}
	}

	if len(toolIndices) <= keepRecentToolCalls {
		return 0, 0
	}

	cutoffIndex := toolIndices[len(toolIndices)-keepRecentToolCalls]
	return c.pruneToolMessagesBefore(cutoffIndex)
}

// Compact prunes bulky tool output from older turns and from all but the latest tool calls.
// It is local and cheap, so it runs before every turn and on every save. Condense, which
// needs the model, replaces older turns with a summary.
func (c *Conversation) Compact() (prunedCount int, bytesSaved int) {
	p1, s1 := c.PruneOldToolOutputs(DefaultKeepRecentTurns)
	p2, s2 := c.PruneInFlightToolOutputs(DefaultKeepRecentToolCalls)
	if p1+p2 > 0 {
		c.Pruned = true
		c.resetTokenMark()
	}
	return p1 + p2, s1 + s2
}

// Condense replaces the turns before the last keepRecentTurns with a summary written by
// summarize, once the conversation has more than DefaultMaxTurnsBeforeSummary user turns.
// It reports whether the transcript changed. When summarize fails, the transcript is left
// as it was and the error is returned.
func (c *Conversation) Condense(ctx context.Context, keepRecentTurns int, summarize Summarizer) (bool, error) {
	if keepRecentTurns < 1 {
		keepRecentTurns = DefaultKeepTurnsAfterSummary
	}
	userIndices := c.userIndices()
	if len(userIndices) <= DefaultMaxTurnsBeforeSummary {
		return false, nil
	}
	return c.condenseOlderTurns(ctx, userIndices, keepRecentTurns, summarize)
}

// condenseOlderTurns replaces the turns before the last keepRecentTurns with a summary of
// them, given the indices of the user messages that start each turn. The summarizer gets
// the system prompt and the older turns; the summary takes the older turns' place as a
// user message followed by a short assistant acknowledgement.
func (c *Conversation) condenseOlderTurns(ctx context.Context, userIndices []int, keepRecentTurns int, summarize Summarizer) (bool, error) {
	if len(userIndices) <= keepRecentTurns {
		return false, nil
	}
	cutoff := userIndices[len(userIndices)-keepRecentTurns]

	// The system prompt at index 0 is kept, not summarized.
	start := 0
	if len(c.Messages) > 0 && c.Messages[0].Role == "system" {
		start = 1
	}
	older := c.Messages[start:cutoff]
	if len(older) == 0 || isSummaryOnly(older) {
		return false, nil
	}

	summary, err := summarize(ctx, c.Messages[:cutoff])
	if err != nil {
		return false, err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return false, ErrEmptySummary
	}

	condensed := make([]chat.Message, 0, len(c.Messages)-len(older)+2)
	condensed = append(condensed, c.Messages[:start]...)
	condensed = append(condensed,
		chat.Message{Role: "user", Content: SummaryPrefix + "\n" + summary},
		chat.Message{Role: "assistant", Content: summaryAck},
	)
	condensed = append(condensed, c.Messages[cutoff:]...)

	c.Messages = condensed
	c.Pruned = true
	c.resetTokenMark()
	return true, nil
}

// EstimateTokens approximates the size of the conversation in tokens at four bytes per
// token, which is close for English prose and somewhat low for code.
// When the provider has reported the prompt size of an earlier request (SetPromptTokens), that
// measured size is the baseline and only the messages appended since are estimated.
func (c *Conversation) EstimateTokens() int {
	if c.LastPromptTokens > 0 && c.TokenMarkIndex >= 0 && c.TokenMarkIndex <= len(c.Messages) {
		return c.LastPromptTokens + messageBytes(c.Messages[c.TokenMarkIndex:])/4
	}
	return messageBytes(c.Messages) / 4
}

// messageBytes counts the content and tool-call bytes of msgs.
func messageBytes(msgs []chat.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return n
}

// FitContext condenses the conversation when its estimated size is over maxTokens. It
// summarizes the older turns once, keeping as many recent turns as fit beside the summary
// (at most DefaultKeepTurnsAfterSummary, at least the latest), then prunes every tool
// output but the latest if the conversation is still too large. It reports whether anything
// changed. A summarizer failure is returned after the pruning has still been tried.
func (c *Conversation) FitContext(ctx context.Context, maxTokens int, summarize Summarizer) (bool, error) {
	changed := false
	var summaryErr error
	if c.EstimateTokens() > maxTokens {
		userIndices := c.userIndices()
		keep := 1
		for k := DefaultKeepTurnsAfterSummary; k > 1; k-- {
			if k < len(userIndices) && messageBytes(c.Messages[userIndices[len(userIndices)-k]:])/4+SummaryTokenAllowance <= maxTokens {
				keep = k
				break
			}
		}
		condensed, err := c.condenseOlderTurns(ctx, userIndices, keep, summarize)
		if err != nil {
			summaryErr = err
		}
		if condensed {
			changed = true
		}
	}
	if c.EstimateTokens() > maxTokens {
		if pruned, _ := c.PruneInFlightToolOutputs(1); pruned > 0 {
			changed = true
			c.Pruned = true
		}
	}
	return changed, summaryErr
}

// isSummaryOnly reports whether msgs are just an earlier summary and its acknowledgement,
// which condensing again would only wrap in another summary.
func isSummaryOnly(msgs []chat.Message) bool {
	return len(msgs) == 2 && msgs[0].Role == "user" && strings.HasPrefix(msgs[0].Content, SummaryPrefix) &&
		msgs[1].Role == "assistant"
}

// userIndices returns the positions of the user messages, which start each turn.
func (c *Conversation) userIndices() []int {
	var indices []int
	for i, m := range c.Messages {
		if m.Role == "user" {
			indices = append(indices, i)
		}
	}
	return indices
}

func (c *Conversation) pruneToolMessagesBefore(cutoffIndex int) (prunedCount int, bytesSaved int) {
	for i := range cutoffIndex {
		msg := &c.Messages[i]
		if msg.Role != "tool" {
			continue
		}

		if len(msg.Content) <= MaxToolOutputChars && strings.Count(msg.Content, "\n") <= MaxToolOutputLines {
			continue
		}

		// Avoid double-pruning: skip if already compacted previously
		if strings.Contains(msg.Content, "pruned for context efficiency") {
			continue
		}

		origLen := len(msg.Content)
		pruned := compactToolOutput(msg.Content)
		if len(pruned) < origLen {
			saved := origLen - len(pruned)
			msg.Content = pruned
			prunedCount++
			bytesSaved += saved
		}
	}
	if prunedCount > 0 {
		c.resetTokenMark()
	}

	return prunedCount, bytesSaved
}

// compactToolOutput retains the head and tail of large tool output while omitting bulky middles.
func compactToolOutput(content string) string {
	// Locate the head and tail by scanning for newlines rather than splitting,
	// so a huge output does not allocate one string per line.
	if lines := strings.Count(content, "\n") + 1; lines > RetainHeadLines+RetainTailLines {
		headEnd := 0
		for range RetainHeadLines {
			headEnd += strings.IndexByte(content[headEnd:], '\n') + 1
		}
		tailStart := len(content)
		for range RetainTailLines {
			tailStart = strings.LastIndexByte(content[:tailStart], '\n')
		}
		omitted := lines - (RetainHeadLines + RetainTailLines)
		return fmt.Sprintf("%s\n... [%d lines pruned for context efficiency] ...\n\n%s", content[:headEnd], omitted, content[tailStart+1:])
	}

	// Fallback for long single-line or few-line content (e.g. minified json, long hex)
	if len(content) > MaxToolOutputChars {
		const headBytes = 400
		const tailBytes = 200
		if headBytes+tailBytes >= len(content) {
			return content
		}

		head := trimPartialRune(content[:headBytes])
		// Step past at most one split rune; invalid bytes beyond it are kept.
		tailStart := len(content) - tailBytes
		for skip := 0; skip < utf8.UTFMax-1 && !utf8.RuneStart(content[tailStart]); skip++ {
			tailStart++
		}
		tail := content[tailStart:]
		omitted := len(content) - len(head) - len(tail)

		return fmt.Sprintf("%s\n\n... [%d bytes pruned for context efficiency] ...\n\n%s", head, omitted, tail)
	}

	return content
}

// trimPartialRune drops a multi-byte rune cut off at the end of s. Invalid
// bytes elsewhere are kept: only the cut can split a rune.
func trimPartialRune(s string) string {
	for i := len(s) - 1; i >= 0 && i >= len(s)-utf8.UTFMax; i-- {
		if utf8.RuneStart(s[i]) {
			if !utf8.FullRuneInString(s[i:]) {
				return s[:i]
			}
			break
		}
	}
	return s
}
