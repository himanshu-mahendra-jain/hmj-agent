package chat

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"

	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/logging"
)

// The limits of a shortened summary request, made when a conversation is too long for the model
// to read whole. hmj does not know the model's context window, so the transcript starts at
// shortenedBudget and is halved each time the provider still finds it too long.
const (
	// shortenedBudget is the size, in bytes, of the first shortened transcript.
	shortenedBudget = 800 << 10
	// shortenedAttempts is how many shortened transcripts are sent before the compaction fails.
	shortenedAttempts = 4
	// maxTranscriptToolBytes caps each string in a tool call's arguments, and each tool result.
	maxTranscriptToolBytes = 1 << 10
	// maxTranscriptCallBytes caps a tool call's arguments as a whole, after their strings are cut.
	maxTranscriptCallBytes = 8 << 10
	// maxTranscriptTextBytes caps the text of each user and assistant message.
	maxTranscriptTextBytes = 16 << 10
)

// shortenedIntro opens the user message that carries a shortened transcript.
const shortenedIntro = "The conversation so far is too long to send whole, so here is a shortened transcript of it. " +
	"Long tool inputs and outputs are cut, and the oldest messages may be left out."

// summarizeShortened compacts messages that are too long for the model: the model summarizes a
// shortened plain-text transcript of them, sent in one user message after the system prompt,
// without tools. Being plain text, the request carries no tool calls and no reasoning blocks, so
// no thinking signature can be broken by what was cut. A transcript the provider still finds too
// long is halved, up to shortenedAttempts times; the last refusal is returned.
func (c *Client) summarizeShortened(ctx context.Context, ref config.ModelRef, messages []Message) (Message, error) {
	var err error
	budget := shortenedBudget
	for range shortenedAttempts {
		var compacted Message
		compacted, err = c.writeSummary(ctx, ref, shortenedSummaryRequest(messages, budget), nil)
		if !IsContextOverflow(err) {
			return compacted, err
		}
		logging.L().Warn("shortened conversation still too long; shortening it further", "budget_bytes", budget, "error", err.Error())
		budget /= 2
	}
	return Message{}, err
}

// shortenedSummaryRequest returns the system prompt of messages, then one user message holding a
// transcript of the other messages in at most budget bytes, and the summary instruction.
func shortenedSummaryRequest(messages []Message, budget int) []Message {
	var request, rest []Message
	for _, m := range messages {
		if m.Role == "system" {
			request = append(request, Message{Role: "system", Content: m.Content})
		} else {
			rest = append(rest, m)
		}
	}
	content := shortenedIntro + "\n\n" + shortenedTranscript(rest, budget) + "\n\n" + summaryInstruction
	return append(request, Message{Role: "user", Content: content})
}

// shortenedTranscript writes messages as plain text, each cut to its limit. Everything up to the
// first user message is kept, so the summary of an earlier compaction and the first request
// survive; of the rest, the newest messages that fit in budget bytes are kept, and a note
// stands in for the ones left out.
func shortenedTranscript(messages []Message, budget int) string {
	toolNames := map[string]string{}
	entries := make([]string, len(messages))
	head := 0
	for i := range messages {
		entries[i] = transcriptEntry(&messages[i], toolNames)
		if head == 0 && messages[i].Role == "user" {
			head = i + 1
		}
	}

	size := 0
	for _, e := range entries[:head] {
		size += len(e) + 2
	}
	start := len(entries)
	for start > head && size+len(entries[start-1])+2 <= budget {
		start--
		size += len(entries[start]) + 2
	}
	kept := entries[:head:head]
	if start > head {
		kept = append(kept, fmt.Sprintf("[%d earlier messages left out]", start-head))
	}
	return strings.Join(append(kept, entries[start:]...), "\n\n")
}

// transcriptEntry writes m as plain text and records the names of the tools it calls in
// toolNames, by call ID, for the results that follow.
func transcriptEntry(m *Message, toolNames map[string]string) string {
	if b := m.CompactionBlock(); b != nil {
		return "[summary of the earlier conversation]\n" + cmp.Or(b.ReadableSummary(), unreadableSummary)
	}
	var b strings.Builder
	if m.Role == "tool" {
		fmt.Fprintf(&b, "[%s result]", cmp.Or(toolNames[m.ToolCallID], "tool"))
		if m.Content != "" {
			b.WriteString("\n" + cutTranscriptText(m.Content, maxTranscriptToolBytes))
		}
	} else {
		fmt.Fprintf(&b, "[%s]", m.Role)
		if m.Content != "" {
			b.WriteString("\n" + cutTranscriptText(m.Content, maxTranscriptTextBytes))
		}
	}
	if len(m.Images) > 0 {
		fmt.Fprintf(&b, "\n[%d images left out]", len(m.Images))
	}
	for _, tc := range m.ToolCalls {
		toolNames[tc.ID] = tc.Function.Name
		fmt.Fprintf(&b, "\n[called %s]\n%s", tc.Function.Name, shortenArguments(tc.Function.Arguments))
	}
	return b.String()
}

// shortenArguments cuts each string in a tool call's JSON arguments to maxTranscriptToolBytes, so
// a short field such as a path survives beside a long one such as a file's content, and then the
// whole to maxTranscriptCallBytes. Arguments that are not valid JSON are cut as text.
func shortenArguments(args string) string {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return cutTranscriptText(args, maxTranscriptToolBytes)
	}
	out, err := json.Marshal(shortenStrings(v), json.Deterministic(true))
	if err != nil {
		return cutTranscriptText(args, maxTranscriptToolBytes)
	}
	return cutTranscriptText(string(out), maxTranscriptCallBytes)
}

// shortenStrings returns v, decoded JSON, with every string cut to maxTranscriptToolBytes.
func shortenStrings(v any) any {
	switch v := v.(type) {
	case string:
		return cutTranscriptText(v, maxTranscriptToolBytes)
	case map[string]any:
		for k, item := range v {
			v[k] = shortenStrings(item)
		}
	case []any:
		for i, item := range v {
			v[i] = shortenStrings(item)
		}
	}
	return v
}

// cutTranscriptText cuts s to at most limit bytes, at a character boundary, with a note of how
// much was left out.
func cutTranscriptText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return fmt.Sprintf("%s[… %d KB left out]", strings.ToValidUTF8(s[:limit], ""), (len(s)-limit+1023)/1024)
}
