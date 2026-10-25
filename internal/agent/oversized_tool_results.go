package agent

import (
	"fmt"
	"slices"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/tools"
)

const (
	// maxToolResultBytes caps any tool result sent to the model. The tools cap their own
	// output well below it; this catches a tool that does not, before one result can fill the
	// context window. It leaves read_file room for its line numbers and notes.
	maxToolResultBytes = tools.MaxReadFileBytes + 64*1024
	// minRemovableResultBytes is the smallest tool result removed to fit a request into the
	// context window; anything shorter would not make room.
	minRemovableResultBytes = 1024
)

// capToolResult cuts output to maxToolResultBytes, at a rune boundary, with a note saying so.
func capToolResult(name, output string) string {
	if len(output) <= maxToolResultBytes {
		return output
	}
	return fmt.Sprintf("%s\n\n[%s result cut to %d KB of %d KB]",
		tools.TruncateToBytes(output, maxToolResultBytes), name, maxToolResultBytes/1024, len(output)/1024)
}

// removeLargestStepResult replaces the largest result of the current step's tool calls, those
// after the last assistant message, with a note, after the provider found the request too long
// for the model. It returns the tool's name and the result's size, and false when no result of
// at least minRemovableResultBytes is left. The results it may change were never part of a
// request that got an answer, so the conversation the model has seen is unchanged; for Claude,
// thinking blocks stay valid.
func removeLargestStepResult(conv *session.Conversation) (string, int, bool) {
	last := -1
	for i, v := range slices.Backward(conv.Messages) {
		if v.Role == "assistant" {
			last = i
			break
		}
	}
	if last < 0 {
		return "", 0, false
	}
	largest := -1
	for i := last + 1; i < len(conv.Messages); i++ {
		m := &conv.Messages[i]
		if m.Role == "tool" && len(m.Content) >= minRemovableResultBytes && (largest < 0 || len(m.Content) > len(conv.Messages[largest].Content)) {
			largest = i
		}
	}
	if largest < 0 {
		return "", 0, false
	}
	result := &conv.Messages[largest]
	name := toolCallName(conv.Messages[last].ToolCalls, result.ToolCallID)
	size := len(result.Content)
	result.Content = fmt.Sprintf("[the %s result (%d KB) was removed: the request was over the model's context limit. "+
		"Ask for less, such as a line range, a grep or a smaller max_lines.]", name, size/1024)
	return name, size, true
}

// toolCallName returns the name of the call with the given ID, or "tool" when none matches.
func toolCallName(calls []chat.ToolCall, id string) string {
	for _, tc := range calls {
		if tc.ID == id {
			return tc.Function.Name
		}
	}
	return "tool"
}

// removedResultNotice tells the user a tool result was dropped to fit the context window.
func removedResultNotice(name string, size int) string {
	return fmt.Sprintf("[removed the %s result (%d KB) from the request: it was over the model's context limit; retrying]", name, size/1024)
}
