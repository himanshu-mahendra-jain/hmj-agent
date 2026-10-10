package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"hmj-agent/internal/agent"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/tools"
)

// bulkyTool returns a large output so that any pruning of it would be visible.
type bulkyTool struct{}

func (bulkyTool) Name() string { return "bulky_tool" }

func (bulkyTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{Type: "function", Function: chat.FunctionDefinition{Name: "bulky_tool"}}
}

func (bulkyTool) Execute(context.Context, string) (string, error) {
	return strings.Repeat("line of output\n", 60), nil
}

// TestToolOutputNotPrunedMidTurn checks that tool output produced earlier in a turn is kept intact
// between tool steps. Rewriting it mid-turn would invalidate the provider's prompt cache.
func TestToolOutputNotPrunedMidTurn(t *testing.T) {
	t.Chdir(t.TempDir())

	toolStep := func(step int) []string {
		return []string{fmt.Sprintf(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i%d","type":"function_call","call_id":"call_%d","name":"bulky_tool"}}`, step, step),
			fmt.Sprintf(`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_%d","delta":"{}"}`, step),
			fmt.Sprintf(`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"i%d","type":"function_call","call_id":"call_%d","name":"bulky_tool","arguments":"{}"}}`, step, step),
			`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		}
	}
	var step atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(step.Add(1))
		chunks := toolStep(n)
		if n > 3 {
			chunks = []string{`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"done"}`}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
		}
	}))
	defer server.Close()

	cfg := mockAgentConfig(server.URL)
	runner := agent.NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{bulkyTool{}})
	conv := &session.Conversation{ID: "c1", Name: "test", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}
	if err := runner.Run(context.Background(), conv, "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var toolMsgs []chat.Message
	for _, m := range conv.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 3 {
		t.Fatalf("expected 3 tool messages, got %d", len(toolMsgs))
	}
	for _, m := range toolMsgs {
		if strings.Contains(m.Content, "pruned for context efficiency") {
			t.Errorf("tool output %s was pruned mid-turn", m.ToolCallID)
		}
	}
}
