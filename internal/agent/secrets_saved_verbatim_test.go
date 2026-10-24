package agent_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/agent"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/tools"
)

// secretEchoTool returns its own arguments, so a secret the model passes in lands in a tool result.
type secretEchoTool struct{}

func (secretEchoTool) Name() string { return "echo_tool" }

func (secretEchoTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{Type: "function", Function: chat.FunctionDefinition{Name: "echo_tool"}}
}

func (secretEchoTool) Execute(_ context.Context, args string) (string, error) {
	return "echoed " + args, nil
}

// TestSecretsSavedVerbatim checks end to end through a Runner that a secret flowing through a tool
// result is kept as it is, in memory and in the saved session file: hmj runs on a dedicated VM and
// masks nothing, so a resumed conversation is the one the model produced.
func TestSecretsSavedVerbatim(t *testing.T) {
	const secret = "sk-live-TOPSECRET-0123456789"
	t.Chdir(t.TempDir())

	toolCall := fmt.Sprintf(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"function_call","call_id":"c1","name":"echo_tool"}}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","call_id":"c1","arguments":"{\"key\":\"%[1]s\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"i1","type":"function_call","call_id":"c1","name":"echo_tool","arguments":"{\"key\":\"%[1]s\"}"}}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`, secret)
	answer := fmt.Sprintf(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"the key is %s"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`, secret)
	var step int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		w.Header().Set("Content-Type", "text/event-stream")
		if step == 1 {
			fmt.Fprint(w, toolCall)
			return
		}
		fmt.Fprint(w, answer)
	}))
	defer server.Close()

	storage := t.TempDir()
	ref := config.ModelRef{Provider: "mock", Model: "mock-model"}
	sm := session.LoadSessionManager(storage, ref)
	conv := sm.Active()

	cfg := mockAgentConfig(server.URL)
	runner := agent.NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{secretEchoTool{}})
	if err := runner.Run(context.Background(), conv, "use my key "+secret); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// In memory: the tool result carries the real secret, verbatim.
	var toolOutput string
	for _, m := range conv.Messages {
		if m.Role == "tool" {
			toolOutput = m.Content
		}
	}
	if !strings.Contains(toolOutput, secret) {
		t.Fatalf("in-memory tool result = %q, want the secret kept verbatim", toolOutput)
	}

	if err := sm.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(storage, "conversations", conv.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), secret) || strings.Contains(string(raw), "[REDACTED]") {
		t.Errorf("saved session does not keep the secret verbatim")
	}
}
