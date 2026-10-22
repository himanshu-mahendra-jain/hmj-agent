package session

import (
	"context"
	"fmt"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
)

// withThinking gives every assistant message of conv a thinking block, a redacted_thinking block
// and an OpenAI reasoning item.
func withThinking(conv *Conversation) *Conversation {
	for i := range conv.Messages {
		if conv.Messages[i].Role == "assistant" {
			conv.Messages[i].Blocks = []chat.Block{
				{Type: "thinking", Text: "t", Signature: "sig"},
				{Type: "redacted_thinking", Data: "d"},
				{Type: "reasoning", Signature: "rs_1", Data: "enc"},
			}
		}
	}
	return conv
}

// thinkingKept reports, for each assistant message in order, whether it still has its
// Anthropic thinking blocks.
func thinkingKept(conv *Conversation) []bool {
	var kept []bool
	for _, m := range conv.Messages {
		if m.Role != "assistant" {
			continue
		}
		has := false
		for _, b := range m.Blocks {
			has = has || b.Type == "thinking" || b.Type == "redacted_thinking"
		}
		kept = append(kept, has)
	}
	return kept
}

func assertKept(t *testing.T, conv *Conversation, want ...bool) {
	t.Helper()
	got := thinkingKept(conv)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("thinking kept per assistant message = %v, want %v", got, want)
	}
}

// TestPruningStripsThinkingAfterTheEdit checks that shortening an older tool output removes the
// Anthropic thinking from every assistant message after it, keeps the thinking before it, and
// leaves text, tool calls and OpenAI reasoning items alone.
func TestPruningStripsThinkingAfterTheEdit(t *testing.T) {
	conv := withThinking(toolTurns(3, 60))
	// Only turn 1's tool output is bulky, so only it is pruned: the call in turn 1, made
	// before its output, keeps its thinking.
	conv.Messages[6].Content = "short"
	conv.Messages[10].Content = "short"

	if pruned, _ := conv.Compact(); pruned != 1 {
		t.Fatalf("Compact pruned %d outputs, want 1", pruned)
	}
	assertKept(t, conv, true, false, false, false, false, false)
	for i, m := range conv.Messages[2:] {
		if m.Role == "assistant" && (len(m.Blocks) != 1 || m.Blocks[0].Type != "reasoning") {
			t.Errorf("message %d blocks = %+v, want only the reasoning item", i+2, m.Blocks)
		}
	}
	if conv.Messages[3].Content != "done 1" || conv.Messages[5].ToolCalls[0].ID != "call_2" {
		t.Error("stripping thinking changed an assistant message's text or tool calls")
	}

	// A second pass that prunes nothing new strips nothing more: thinking produced after the
	// edit was made against the edited history and stays valid.
	conv.Messages = append(conv.Messages, withThinking(&Conversation{Messages: []chat.Message{
		{Role: "user", Content: "turn 4"}, {Role: "assistant", Content: "done 4"},
	}}).Messages...)
	if pruned, _ := conv.Compact(); pruned != 0 {
		t.Fatalf("second Compact pruned %d outputs, want 0", pruned)
	}
	assertKept(t, conv, true, false, false, false, false, false, true)
}

// TestNoEditsWhileInAToolRound checks that a transcript ending in an open tool round with
// thinking, as a halted turn leaves it, is neither pruned nor condensed, so the round's thinking is
// sent back as it was, and that a round without thinking is still pruned.
func TestNoEditsWhileInAToolRound(t *testing.T) {
	conv := withThinking(toolTurns(7, 60))
	conv.Messages = append(conv.Messages, withThinking(&Conversation{Messages: []chat.Message{
		{Role: "user", Content: "turn 8"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_8"}}},
		{Role: "tool", ToolCallID: "call_8", Content: CancelledToolResult},
	}}).Messages...)
	before := fmt.Sprint(conv.Messages)

	if pruned, _ := conv.Compact(); pruned != 0 {
		t.Errorf("Compact pruned %d outputs during a tool round, want 0", pruned)
	}
	summarize := func(context.Context, []chat.Message) (string, error) { return "summary", nil }
	if condensed, err := conv.Condense(context.Background(), 3, summarize); condensed || err != nil {
		t.Errorf("Condense = (%v, %v) during a tool round, want (false, nil)", condensed, err)
	}
	if fmt.Sprint(conv.Messages) != before {
		t.Error("the transcript changed during a tool round")
	}

	// Once a new prompt is appended the round is still open, since the prompt is sent with
	// its tool results.
	conv.Messages = append(conv.Messages, chat.Message{Role: "user", Content: "next"})
	if pruned, _ := conv.PruneInFlightToolOutputs(1); pruned != 0 {
		t.Errorf("PruneInFlightToolOutputs pruned %d outputs after a halted round, want 0", pruned)
	}

	// Without thinking in the round, as with the other protocols, there is nothing to keep.
	conv.Messages[len(conv.Messages)-3].Blocks = nil
	if pruned, _ := conv.PruneInFlightToolOutputs(1); pruned == 0 {
		t.Error("a tool round without thinking was not pruned")
	}
}

// TestCondenseStripsThinkingFromKeptTurns checks that the turns kept after a summary lose their
// thinking, which was produced after the turns the summary replaced.
func TestCondenseStripsThinkingFromKeptTurns(t *testing.T) {
	conv := withThinking(chatTurns(7))
	summarize := func(context.Context, []chat.Message) (string, error) { return "summary", nil }
	if condensed, err := conv.Condense(context.Background(), 3, summarize); !condensed || err != nil {
		t.Fatalf("Condense = (%v, %v), want (true, nil)", condensed, err)
	}
	// The summary acknowledgement, then the three kept answers.
	assertKept(t, conv, false, false, false, false)
}

// TestLoadRepairsThinkingInPrunedSessions checks that a pruned conversation saved by an earlier
// version, which kept thinking after its edits, loses its thinking once on load, apart from an
// open tool round's; that an unpruned one keeps it; and that a conversation already marked, or
// one created now, is not repaired again.
func TestLoadRepairsThinkingInPrunedSessions(t *testing.T) {
	t.Chdir(t.TempDir())
	thinking := `"blocks":[{"type":"thinking","text":"t","signature":"s"}]`
	conv := func(id, flags, last string) string {
		return `"` + id + `":{"id":"` + id + `","name":"c","model":{"provider":"p","model":"m"}` + flags + `,"messages":[
			{"role":"system","content":"s"},{"role":"user","content":"u"},
			{"role":"assistant","content":"a",` + thinking + `},{"role":"user","content":"u2"},` + last + `]}`
	}
	answer := `{"role":"assistant","content":"b",` + thinking + `}`
	openRound := `{"role":"assistant",` + thinking + `,"tool_calls":[{"id":"c1","type":"function","function":{"name":"list_files","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}`
	path := writeSessionFile(t, `{"activeId":"1","order":["1","2","3","4"],"conversations":{`+
		conv("1", `,"pruned":true`, answer)+`,`+
		conv("2", `,"pruned":true`, openRound)+`,`+
		conv("3", ``, answer)+`,`+
		conv("4", `,"pruned":true,"thinkingKeptValid":true`, answer)+`}}`)
	sm := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})

	assertKept(t, sm.Conversations["1"], false, false)
	assertKept(t, sm.Conversations["2"], false, true)
	assertKept(t, sm.Conversations["3"], true, true)
	assertKept(t, sm.Conversations["4"], true, true)
	for id, c := range sm.Conversations {
		if !c.ThinkingKeptValid {
			t.Errorf("conversation %s not marked after load", id)
		}
	}
	if !sm.New("fresh").ThinkingKeptValid {
		t.Error("a new conversation is not marked")
	}
}
