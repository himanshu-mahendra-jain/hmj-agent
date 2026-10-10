package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
)

func bulkyOutput(label string, lines int) string {
	var sb strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&sb, "%s line %d with some padding text to make it long\n", label, i)
	}
	return sb.String()
}

// toolTurns builds n turns, each a user prompt, a tool call, a bulky tool result and an answer.
func toolTurns(n, lines int) *Conversation {
	conv := &Conversation{ID: "c", Name: "c"}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("call_%d", i)
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("turn %d", i)},
			chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: id}}},
			chat.Message{Role: "tool", ToolCallID: id, Content: bulkyOutput(fmt.Sprintf("out%d", i), lines)},
			chat.Message{Role: "assistant", Content: fmt.Sprintf("done %d", i)},
		)
	}
	return conv
}

func TestCompactPrunesOldToolOutputs(t *testing.T) {
	conv := toolTurns(3, 60)
	var before []int
	for _, m := range conv.Messages {
		before = append(before, len(m.Content))
	}

	pruned, bytesSaved := conv.Compact()

	changed, saved := 0, 0
	for i, m := range conv.Messages {
		if len(m.Content) < before[i] {
			changed++
			saved += before[i] - len(m.Content)
			if m.Role != "tool" {
				t.Errorf("message %d (%s) shrank; only tool outputs should", i, m.Role)
			}
		}
	}
	if changed == 0 {
		t.Fatal("no tool output was pruned")
	}
	if pruned != changed || bytesSaved != saved {
		t.Errorf("Compact() = (%d, %d), want (%d, %d)", pruned, bytesSaved, changed, saved)
	}
	// The newest tool output stays intact; an old one keeps its head and tail.
	last := conv.Messages[len(conv.Messages)-2].Content
	if last != bulkyOutput("out3", 60) {
		t.Error("most recent tool output must not be pruned")
	}
	first := conv.Messages[2].Content
	if !strings.HasPrefix(first, "out1 line 1 ") || !strings.Contains(first, "out1 line 60 ") {
		t.Errorf("pruned output lost its head or tail: %q", first)
	}
	if !conv.Pruned {
		t.Error("conversation not marked as pruned")
	}
}

// Compact only prunes tool output. Summarizing older turns needs the model, so it is
// Condense's job.
func TestCompactDoesNotSummarize(t *testing.T) {
	conv := &Conversation{ID: "c", Name: "c"}
	for i := 1; i <= 9; i++ {
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("question %d %s", i, strings.Repeat("x", 400))},
			chat.Message{Role: "assistant", Content: fmt.Sprintf("answer %d %s", i, strings.Repeat("y", 400))},
		)
	}
	if pruned, saved := conv.Compact(); pruned != 0 || saved != 0 || len(conv.Messages) != 18 {
		t.Errorf("Compact() = (%d, %d) and %d messages, want nothing changed", pruned, saved, len(conv.Messages))
	}
}

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

func TestCondense(t *testing.T) {
	t.Run("replaces older turns with the model's summary", func(t *testing.T) {
		conv := chatTurns(7)
		conv.SetPromptTokens(5000)
		var got []chat.Message
		summarize := func(_ context.Context, history []chat.Message) (string, error) {
			got = slices.Clone(history)
			return "  THE SUMMARY\n", nil
		}
		condensed, err := conv.Condense(context.Background(), 3, summarize)
		if err != nil || !condensed {
			t.Fatalf("Condense = (%v, %v), want (true, nil)", condensed, err)
		}
		// The summarizer sees the system prompt and the four older turns, not the three kept.
		if len(got) != 9 || got[0].Content != "SYSTEM" || got[1].Content != "question 1" || got[8].Content != "answer 4" {
			t.Errorf("summarizer got %d messages, first %q last %q", len(got), got[0].Content, got[len(got)-1].Content)
		}
		want := []string{"SYSTEM", SummaryPrefix + "\nTHE SUMMARY", summaryAck, "question 5", "answer 5", "question 6", "answer 6", "question 7", "answer 7"}
		var contents []string
		for _, m := range conv.Messages {
			contents = append(contents, m.Content)
		}
		if !slices.Equal(contents, want) {
			t.Errorf("messages after condense:\n%q\nwant:\n%q", contents, want)
		}
		if conv.Messages[1].Role != "user" || conv.Messages[2].Role != "assistant" {
			t.Errorf("summary roles = %s, %s, want user, assistant", conv.Messages[1].Role, conv.Messages[2].Role)
		}
		if !conv.Pruned || conv.LastPromptTokens != 0 || conv.TokenMarkIndex != 0 {
			t.Error("condensing must mark the conversation pruned and forget the reported prompt size")
		}
	})
	t.Run("short conversations are not summarized", func(t *testing.T) {
		conv := chatTurns(DefaultMaxTurnsBeforeSummary)
		called := false
		summarize := func(context.Context, []chat.Message) (string, error) { called = true; return "s", nil }
		if condensed, err := conv.Condense(context.Background(), 3, summarize); condensed || err != nil || called {
			t.Errorf("Condense = (%v, %v), called=%v; want nothing done", condensed, err, called)
		}
	})
	t.Run("a failed summary leaves the transcript alone", func(t *testing.T) {
		conv := chatTurns(7)
		before := slices.Clone(conv.Messages)
		boom := errors.New("boom")
		summarize := func(context.Context, []chat.Message) (string, error) { return "", boom }
		condensed, err := conv.Condense(context.Background(), 3, summarize)
		if condensed || !errors.Is(err, boom) || !reflect.DeepEqual(conv.Messages, before) {
			t.Errorf("Condense = (%v, %v), messages changed=%v", condensed, err, !reflect.DeepEqual(conv.Messages, before))
		}
	})
	t.Run("an empty summary is an error", func(t *testing.T) {
		conv := chatTurns(7)
		before := slices.Clone(conv.Messages)
		summarize := func(context.Context, []chat.Message) (string, error) { return " \n ", nil }
		condensed, err := conv.Condense(context.Background(), 3, summarize)
		if condensed || !errors.Is(err, ErrEmptySummary) || !reflect.DeepEqual(conv.Messages, before) {
			t.Errorf("Condense = (%v, %v)", condensed, err)
		}
	})
	t.Run("condensing again summarizes the summary and the turns since", func(t *testing.T) {
		conv := chatTurns(7)
		summarize := func(_ context.Context, history []chat.Message) (string, error) {
			return fmt.Sprintf("summary of %d messages", len(history)), nil
		}
		if _, err := conv.Condense(context.Background(), 3, summarize); err != nil {
			t.Fatal(err)
		}
		for i := 8; i <= 10; i++ {
			conv.Messages = append(conv.Messages,
				chat.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
				chat.Message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)})
		}
		condensed, err := conv.Condense(context.Background(), 3, summarize)
		if err != nil || !condensed {
			t.Fatalf("second Condense = (%v, %v)", condensed, err)
		}
		// system, old summary, ack, turns 5-7 = 9 messages went to the summarizer.
		if conv.Messages[1].Content != SummaryPrefix+"\nsummary of 9 messages" || len(conv.Messages) != 9 {
			t.Errorf("after second condense: %d messages, summary %q", len(conv.Messages), conv.Messages[1].Content)
		}
	})
	t.Run("a summary followed only by kept turns is not re-wrapped", func(t *testing.T) {
		conv := chatTurns(7)
		calls := 0
		summarize := func(context.Context, []chat.Message) (string, error) { calls++; return "s", nil }
		conv.Condense(context.Background(), 1, summarize)
		// Now: system, summary, ack, turn 7. Keeping 1 turn, the older part is the summary alone.
		if condensed, err := conv.Condense(context.Background(), 1, summarize); condensed || err != nil || calls != 1 {
			t.Errorf("re-condense = (%v, %v), summarizer calls = %d, want nothing done", condensed, err, calls)
		}
	})
}

func TestCompactAlreadyOptimal(t *testing.T) {
	conv := toolTurns(1, 3)
	snapshot := slices.Clone(conv.Messages)
	if pruned, saved := conv.Compact(); pruned != 0 || saved != 0 {
		t.Errorf("Compact() = (%d, %d), want (0, 0)", pruned, saved)
	}
	if !slices.EqualFunc(snapshot, conv.Messages, func(a, b chat.Message) bool { return a.Content == b.Content }) || conv.Pruned {
		t.Error("an optimal conversation must be left untouched")
	}
}
