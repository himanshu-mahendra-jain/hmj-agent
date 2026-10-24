package agent_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"hmj-agent/internal/agent"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/tools"
)

// haltingTool cancels the turn while it runs, simulating the user pressing Esc during a tool call.
type haltingTool struct {
	cancel context.CancelFunc
	calls  int
}

func (h *haltingTool) Name() string { return "halting_tool" }

func (h *haltingTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{Type: "function", Function: chat.FunctionDefinition{Name: "halting_tool"}}
}

func (h *haltingTool) Execute(context.Context, string) (string, error) {
	h.calls++
	h.cancel()
	return "ran", nil
}

// TestHaltAnswersEveryToolCall checks that halting a turn during the first of two tool calls
// still leaves a tool result for each call, so the transcript stays valid for the provider.
func TestHaltAnswersEveryToolCall(t *testing.T) {
	t.Chdir(t.TempDir())

	chunks := []string{
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_a","type":"function_call","call_id":"call_a","name":"halting_tool"}}`,
		`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_a","delta":"{}"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_a","type":"function_call","call_id":"call_a","name":"halting_tool","arguments":"{}"}}`,
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"item_b","type":"function_call","call_id":"call_b","name":"halting_tool"}}`,
		`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_b","delta":"{}"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"id":"item_b","type":"function_call","call_id":"call_b","name":"halting_tool","arguments":"{}"}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool := &haltingTool{cancel: cancel}

	cfg := mockAgentConfig(server.URL)
	runner := agent.NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{tool})
	conv := &session.Conversation{ID: "c1", Name: "test", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}

	if err := runner.Run(ctx, conv, "hi"); err != nil {
		t.Fatalf("a halted turn must end without error, got %v", err)
	}
	if tool.calls != 1 {
		t.Fatalf("expected only the first tool to run, got %d runs", tool.calls)
	}

	var toolMsgs []chat.Message
	for _, m := range conv.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("expected a tool message for each of the 2 tool calls, got %d", len(toolMsgs))
	}
	if toolMsgs[0].ToolCallID != "call_a" || toolMsgs[1].ToolCallID != "call_b" {
		t.Errorf("tool results answer %q and %q, want call_a and call_b", toolMsgs[0].ToolCallID, toolMsgs[1].ToolCallID)
	}
	if toolMsgs[1].Content != session.CancelledToolResult {
		t.Errorf("unanswered call content = %q, want the cancelled placeholder", toolMsgs[1].Content)
	}
}
