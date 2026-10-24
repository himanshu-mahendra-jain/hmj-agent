package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/tools"
)

// scriptedTool returns its outputs in turn, one per call.
type scriptedTool struct {
	outputs []string
	calls   *atomic.Int32
}

func (scriptedTool) Name() string                    { return "slow_tool" }
func (scriptedTool) Definition() chat.ToolDefinition { return chat.ToolDefinition{} }
func (s scriptedTool) Execute(context.Context, string) (string, error) {
	return s.outputs[s.calls.Add(1)-1], nil
}

// TestRepeatedCallIsNoted runs a turn in which the model sends a call, the same call again with
// its arguments spaced differently, the call with another argument, then that call again with a
// different result, and checks that only the first repeat's result tells the model it is the
// same call with the same result.
func TestRepeatedCallIsNoted(t *testing.T) {
	args := []string{`{"path":"a","n":1}`, `{"n": 1, "path": "a"}`, `{"path":"a","n":2}`, `{"path":"a","n":2}`}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data) }
		if n := int(requests.Add(1)); n <= len(args) {
			send("response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":"slow_tool"}}`, n, n))
			send("response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_%d","delta":%q}`, n, args[n-1]))
		} else {
			send("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Done."}`)
		}
		send("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
	}}
	runner := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{scriptedTool{[]string{"x", "x", "x", "y"}, new(atomic.Int32)}})
	conv := &session.Conversation{Model: config.ModelRef{Provider: "p", Model: "m"}}
	captureOutput(t, func() {
		if err := runner.Run(context.Background(), conv, "question"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	var results []string
	for _, m := range conv.Messages {
		if m.Role == "tool" {
			results = append(results, m.Content)
		}
	}
	if want := []string{"x", "x" + repeatedCallNote, "x", "y"}; strings.Join(results, "|") != strings.Join(want, "|") {
		t.Errorf("tool results = %q, want %q", results, want)
	}
}
