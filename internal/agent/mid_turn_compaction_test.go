package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/tools"
)

// toolCallReplyAt answers with one call to the named tool and reports a prompt of inputTokens.
func toolCallReplyAt(name, args string, inputTokens int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"item_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":%q,\"arguments\":%q}}\n\n", name, args)
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":2,\"total_tokens\":%d}}}\n\n", inputTokens, inputTokens+2)
	}
}

// compactRequests counts the requests made to the compact endpoint, which carry no "stream" field.
func compactRequests(p *scriptedProvider) int {
	n := 0
	for _, req := range p.Requests() {
		if !strings.Contains(req, `"stream"`) {
			n++
		}
	}
	return n
}

// TestLongTurnIsCompactedBetweenSteps checks that a turn whose prompt passes the limit is compacted
// before its next step, which goes on from the compaction and the note to continue, so one long
// turn cannot fill the model's context window.
func TestLongTurnIsCompactedBetweenSteps(t *testing.T) {
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t,
		toolCallReplyAt("big", "{}", session.CompactAtTokens+1),
		textReply("done"))
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{bigTool{size: 1024}})
	conv := &session.Conversation{ID: "1", Model: mockModel}

	var err error
	out := captureOutput(t, func() { err = r.Run(context.Background(), conv, "do the long job") })

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "[compacted the conversation:") {
		t.Errorf("the compaction was not reported:\n%s", out)
	}
	reqs := p.Requests()
	if len(reqs) != 3 || compactRequests(p) != 1 {
		t.Fatalf("%d requests (%d compact), want a step, a compaction and a step", len(reqs), compactRequests(p))
	}
	if last := reqs[2]; !strings.Contains(last, "ENC") || !strings.Contains(last, "compacted while you were working") || strings.Contains(last, "do the long job") {
		t.Errorf("the step after the compaction should carry the window and the note, not the replaced prompt:\n%s", last)
	}
	if !conv.IsCompacted() || len(conv.Messages) != 4 ||
		conv.Messages[2].Content != session.ContinueAfterCompaction || conv.Messages[3].Content != "done" {
		t.Errorf("conversation = %+v, want system, compaction, note, answer", conv.Messages)
	}
}

// TestFailedMidTurnCompactionWaitsBeforeRetrying checks that a compaction that fails during a turn
// is reported, the turn goes on, and the next attempt waits until the conversation has grown by
// midTurnRetryTokens, so a failing provider is not asked at every step.
func TestFailedMidTurnCompactionWaitsBeforeRetrying(t *testing.T) {
	t.Chdir(t.TempDir())
	over := session.CompactAtTokens + 1
	p := newScriptedProvider(t,
		toolCallReplyAt("big", "{}", over),                         // fails to compact
		toolCallReplyAt("big", "{}", over+1000),                    // too soon to try again
		toolCallReplyAt("big", "{}", over+midTurnRetryTokens+5000), // tried again
		textReply("done"))
	bad := statusReply(http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	p.QueueCompact(bad, bad)
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{bigTool{size: 1024}})
	r.Client.RetryConfig = chat.RetryConfig{}
	conv := &session.Conversation{ID: "1", Model: mockModel}

	var err error
	out := captureOutput(t, func() { err = r.Run(context.Background(), conv, "do the long job") })

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := compactRequests(p); n != 2 {
		t.Errorf("%d compact requests, want 2", n)
	}
	if !strings.Contains(out, "[could not compact the conversation: ") || !strings.Contains(out, "the turn goes on]") {
		t.Errorf("the failure was not reported:\n%s", out)
	}
	if conv.IsCompacted() || conv.Messages[len(conv.Messages)-1].Content != "done" {
		t.Errorf("the turn should end uncompacted with its answer: %+v", conv.Messages[len(conv.Messages)-1])
	}
}

// TestOverflowWithNothingToRemoveIsCompacted checks that a step the provider refuses as too long,
// with no large result of its own to remove, has the conversation compacted and is sent again:
// before the model has replied, all but the turn's prompt; later, the whole turn. A first prompt
// refused as too long, with nothing before it, is not summarized away.
func TestOverflowWithNothingToRemoveIsCompacted(t *testing.T) {
	t.Run("history fills the window", func(t *testing.T) {
		t.Chdir(t.TempDir())
		p := newScriptedProvider(t, overflowReply, textReply("done"))
		cfg := p.config()
		r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
		conv := largeConversation()
		conv.SetPromptTokens(200_000) // under the limit, as on a model with a smaller window

		var err error
		out := captureOutput(t, func() { err = r.Run(context.Background(), conv, "one more") })

		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if n, c := len(p.Requests()), compactRequests(p); n != 3 || c != 1 {
			t.Errorf("%d requests (%d compact), want the refused step, a compaction and the step again", n, c)
		}
		// The turn had no reply yet, so its prompt is kept as written, with no note to continue.
		if !conv.IsCompacted() || len(conv.Messages) != 4 || conv.Messages[2].Content != "one more" ||
			conv.Messages[3].Content != "done" || !strings.Contains(out, "[compacted the conversation:") {
			t.Errorf("conversation = %+v, want system, compaction, the prompt and the answer", conv.Messages)
		}
		if retry := p.Requests()[2]; !strings.Contains(retry, `"one more"`) || strings.Contains(retry, "old request") {
			t.Errorf("the step sent again should carry the prompt, not the compacted turns:\n%s", retry)
		}
	})

	t.Run("later step", func(t *testing.T) {
		t.Chdir(t.TempDir())
		p := newScriptedProvider(t, toolCallReply("big", "{}"), overflowReply, textReply("done"))
		cfg := p.config()
		r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{bigTool{size: 64}})
		conv := &session.Conversation{ID: "1", Model: mockModel}

		var err error
		captureOutput(t, func() { err = r.Run(context.Background(), conv, "read the big thing") })

		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if n, c := len(p.Requests()), compactRequests(p); n != 4 || c != 1 {
			t.Errorf("%d requests (%d compact), want a step, the refused step, a compaction and the step again", n, c)
		}
		if !conv.IsCompacted() || len(conv.Messages) != 4 || conv.Messages[2].Content != session.ContinueAfterCompaction {
			t.Errorf("conversation = %+v, want system, compaction, note, answer", conv.Messages)
		}
	})

	t.Run("first prompt", func(t *testing.T) {
		t.Chdir(t.TempDir())
		p := newScriptedProvider(t, overflowReply)
		cfg := p.config()
		r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
		r.Client.RetryConfig = chat.RetryConfig{}
		conv := &session.Conversation{ID: "1", Model: mockModel}

		var err error
		captureOutput(t, func() { err = r.Run(context.Background(), conv, "a huge paste") })

		if !chat.IsContextOverflow(err) {
			t.Errorf("err = %v, want the provider's refusal", err)
		}
		if n := compactRequests(p); n != 0 || conv.IsCompacted() {
			t.Errorf("a first prompt was compacted (%d compact requests)", n)
		}
	})
}

// TestResumedTranscriptHidesTheContinueNote checks that a resumed conversation compacted mid-turn
// shows the compaction, not hmj's note to the model as if the user had typed it.
func TestResumedTranscriptHidesTheContinueNote(t *testing.T) {
	conv := &session.Conversation{ID: "1", Model: mockModel, Messages: []chat.Message{
		{Role: "system", Content: "system"},
		{Role: "assistant", Blocks: []chat.Block{{Type: chat.BlockSummary, Text: "summary of the work"}}},
		{Role: "user", Content: session.ContinueAfterCompaction},
		{Role: "assistant", Content: "done"},
	}}

	out := captureOutput(t, func() { PrintConversationHistory(conv) })

	if strings.Contains(out, "compacted while you were working") || strings.Contains(out, "you:") {
		t.Errorf("the note was shown as a prompt:\n%s", out)
	}
	if !strings.Contains(out, "[the earlier conversation was compacted]") || !strings.Contains(out, "done") {
		t.Errorf("the compaction and the answer should show:\n%s", out)
	}
}
