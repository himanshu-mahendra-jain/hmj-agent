package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/tools"
)

// bigTool is a tool named "big" whose result is size bytes of text.
type bigTool struct{ size int }

func (b bigTool) Name() string { return "big" }
func (b bigTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{Type: "function", Function: chat.FunctionDefinition{
		Name: "big", Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}}
}
func (b bigTool) Execute(context.Context, string) (string, error) {
	return strings.Repeat("payload ", b.size/8), nil
}

// overflowReply is the OpenAI Responses answer to a prompt over the context window.
var overflowReply = statusReply(http.StatusBadRequest,
	`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_length_exceeded"}}`)

// runBigToolTurn runs one turn in which the model calls bigTool once, against a provider that
// gives the replies after the tool call.
func runBigToolTurn(t *testing.T, size int, replies ...http.HandlerFunc) (*scriptedProvider, *session.Conversation, string, error) {
	t.Helper()
	t.Chdir(t.TempDir())
	p := newScriptedProvider(t, append([]http.HandlerFunc{toolCallReply("big", "{}")}, replies...)...)
	cfg := p.config()
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{bigTool{size: size}})
	conv := &session.Conversation{ID: "1", Model: mockModel}
	var err error
	out := captureOutput(t, func() { err = r.Run(context.Background(), conv, "read the big thing") })
	return p, conv, out, err
}

// toolResult returns the content of the conversation's only tool result.
func toolResult(t *testing.T, conv *session.Conversation) string {
	t.Helper()
	for _, m := range conv.Messages {
		if m.Role == "tool" {
			return m.Content
		}
	}
	t.Fatal("no tool result in the conversation")
	return ""
}

// A request over the context limit is sent again without the step's large tool result, and the
// turn goes on.
func TestOverflowRemovesTheLargeResultAndRetries(t *testing.T) {
	p, conv, out, err := runBigToolTurn(t, 100*1024, overflowReply, textReply("done"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := toolResult(t, conv); !strings.HasPrefix(got, "[the big result (100 KB) was removed: the request was over the model's context limit.") {
		t.Errorf("tool result is %q", got[:min(len(got), 200)])
	}
	if !strings.Contains(out, "[removed the big result (100 KB) from the request: it was over the model's context limit; retrying]") {
		t.Errorf("no notice in output %q", out)
	}
	reqs := p.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(reqs))
	}
	if strings.Contains(reqs[2], "payload payload") || !strings.Contains(reqs[1], "payload payload") {
		t.Errorf("the retry should leave out the result the first try sent")
	}
	if last := conv.Messages[len(conv.Messages)-1]; last.Role != "assistant" || last.Content != "done" {
		t.Errorf("turn ended with %+v", last)
	}
}

// When the request is still too long with nothing large left to remove, the error is reported,
// and the conversation keeps the removal, so the saved conversation can be sent again.
func TestOverflowThatPersistsKeepsTheRemoval(t *testing.T) {
	p, conv, _, err := runBigToolTurn(t, 100*1024, overflowReply, overflowReply)
	if !chat.IsContextOverflow(err) {
		t.Fatalf("Run returned %v, want the overflow error", err)
	}
	if got := toolResult(t, conv); !strings.HasPrefix(got, "[the big result (100 KB) was removed") {
		t.Errorf("tool result is %q", got[:min(len(got), 200)])
	}
	if n := len(p.Requests()); n != 3 {
		t.Errorf("%d requests, want 3: one removal, then no large result is left", n)
	}
	if last := conv.Messages[len(conv.Messages)-1]; !strings.HasPrefix(last.Content, "[error: ") {
		t.Errorf("turn ended with %+v", last)
	}
}

// Any other HTTP 400 is reported as before, with the tool result kept.
func TestOtherBadRequestKeepsTheResult(t *testing.T) {
	bad := statusReply(http.StatusBadRequest, `{"error":{"message":"Invalid schema for function 'big'.","type":"invalid_request_error"}}`)
	p, conv, _, err := runBigToolTurn(t, 100*1024, bad)
	if err == nil || chat.IsContextOverflow(err) {
		t.Fatalf("Run returned %v", err)
	}
	if got := toolResult(t, conv); !strings.HasPrefix(got, "payload payload") {
		t.Errorf("tool result changed to %q", got[:min(len(got), 200)])
	}
	if n := len(p.Requests()); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
}

// A tool result over maxToolResultBytes is cut before it reaches the model.
func TestToolResultIsCapped(t *testing.T) {
	_, conv, out, err := runBigToolTurn(t, 400*1024, textReply("done"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "[big result cut to 320 KB of 400 KB]") {
		t.Error("the terminal does not show the cut result the model gets")
	}
	got := toolResult(t, conv)
	if !strings.HasSuffix(got, "\n\n[big result cut to 320 KB of 400 KB]") {
		t.Errorf("result ends %q", got[max(0, len(got)-100):])
	}
	if len(got) > maxToolResultBytes+100 {
		t.Errorf("result is %d bytes", len(got))
	}
}

// Only results of the current step are removed, and the small ones are left alone.
func TestRemoveLargestStepResultChoosesTheLargestOfTheStep(t *testing.T) {
	big := strings.Repeat("x", 50*1024)
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "old", Function: chat.ToolCallFunction{Name: "read_file"}}}},
		{Role: "tool", ToolCallID: "old", Content: big + big}, // an earlier step: already answered
		{Role: "assistant", ToolCalls: []chat.ToolCall{
			{ID: "a", Function: chat.ToolCallFunction{Name: "run_command"}},
			{ID: "b", Function: chat.ToolCallFunction{Name: "fetch_url"}},
			{ID: "c", Function: chat.ToolCallFunction{Name: "list_files"}},
		}},
		{Role: "tool", ToolCallID: "a", Content: big[:2048]},
		{Role: "tool", ToolCallID: "b", Content: big},
		{Role: "tool", ToolCallID: "c", Content: "small"},
	}}

	for _, want := range []string{"fetch_url", "run_command"} {
		name, _, ok := removeLargestStepResult(conv)
		if !ok || name != want {
			t.Fatalf("removed %q (%v), want %q", name, ok, want)
		}
	}
	if _, _, ok := removeLargestStepResult(conv); ok {
		t.Error("removed a result under minRemovableResultBytes")
	}
	if conv.Messages[2].Content != big+big || conv.Messages[6].Content != "small" {
		t.Error("changed a result outside the step, or a small one")
	}
}

func TestOverflowHint(t *testing.T) {
	err := &chat.StatusError{Code: http.StatusBadRequest, Body: "invalid_request_error: prompt is too long: 1200054 tokens > 1000000 maximum"}
	if got := RemediationHint(err); !strings.Contains(got, "/new") {
		t.Errorf("hint is %q", got)
	}
	if got := RemediationHint(&chat.StatusError{Code: http.StatusBadRequest, Body: "bad"}); got != "" {
		t.Errorf("hint for another 400 is %q", got)
	}
}
