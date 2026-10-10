package session

import (
	"testing"

	"hmj-agent/internal/chat"
)

func toolCallAssistant(ids ...string) chat.Message {
	m := chat.Message{Role: "assistant"}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, chat.ToolCall{ID: id, Type: "function", Function: chat.ToolCallFunction{Name: "t"}})
	}
	return m
}

// TestRepairToolCallsAnswersOrphans checks that a tool call with no result gets a placeholder right
// after the results that do exist, and complete transcripts are returned untouched.
func TestRepairToolCallsAnswersOrphans(t *testing.T) {
	orphaned := []chat.Message{
		{Role: "user", Content: "go"},
		toolCallAssistant("a", "b"),
		{Role: "tool", ToolCallID: "a", Content: "done"},
		{Role: "user", Content: "next"},
	}
	got := repairToolCalls(orphaned)
	if len(got) != 5 {
		t.Fatalf("got %d messages, want 5: %+v", len(got), got)
	}
	if got[3].Role != "tool" || got[3].ToolCallID != "b" || got[3].Content != CancelledToolResult {
		t.Errorf("placeholder = %+v, want a cancelled result for b", got[3])
	}
	if got[4].Role != "user" {
		t.Errorf("message after the repaired block = %+v, want the user message", got[4])
	}

	complete := []chat.Message{{Role: "user", Content: "x"}, toolCallAssistant("a"), {Role: "tool", ToolCallID: "a", Content: "ok"}}
	if out := repairToolCalls(complete); len(out) != len(complete) {
		t.Errorf("complete transcript changed: %+v", out)
	}
}

// TestRepairToolCallsTrailingCall checks a call left unanswered at the very end of the transcript.
func TestRepairToolCallsTrailingCall(t *testing.T) {
	msgs := []chat.Message{{Role: "user", Content: "x"}, toolCallAssistant("only")}
	got := repairToolCalls(msgs)
	if len(got) != 3 || got[2].ToolCallID != "only" {
		t.Fatalf("got %+v, want a placeholder for the trailing call", got)
	}
}
