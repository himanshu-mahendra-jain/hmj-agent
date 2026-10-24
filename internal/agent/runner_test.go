package agent_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hmj-agent/internal/agent"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/logging"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
	"hmj-agent/internal/tools"
)

type panickingMockTool struct{}

func (p *panickingMockTool) Name() string { return "panicking_tool" }

func (p *panickingMockTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name: "panicking_tool",
		},
	}
}

func (p *panickingMockTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	panic("critical slice out of bounds error in tool")
}

// TestToolPanicRecoveryInRunner drives the production Runner.Run loop end to
// end: the model requests a tool whose Execute panics, and the runner's own
// recover() must convert the panic into tool output instead of crashing the
// process, allowing the conversation to continue to the final turn.
func TestToolPanicRecoveryInRunner(t *testing.T) {
	t.Chdir(t.TempDir())

	// Step 1: model calls the panicking tool; step 2: model finishes.
	var step atomic.Int32
	chunksA := []string{
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_panic_1","name":"panicking_tool"}}`,
		`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_panic_1","delta":"{}"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_panic_1","name":"panicking_tool","arguments":"{}"}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}
	chunksB := []string{
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"done"}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := chunksA
		if step.Add(1) > 1 {
			current = chunksB
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range current {
			fmt.Fprintf(w, "%s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := mockAgentConfig(server.URL)
	client := chat.NewClient(cfg)
	runner := agent.NewRunnerWithTools(client, cfg, []tools.Tool{&panickingMockTool{}})
	conv := &session.Conversation{ID: "c1", Name: "test", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}

	// A panicking tool must never escape Runner.Run: the production recover
	// records it as a tool result error and the loop continues.
	if err := runner.Run(context.Background(), conv, "hi"); err != nil {
		t.Fatalf("runner must survive a tool panic, got error: %v", err)
	}

	var toolMsgs []chat.Message
	for _, m := range conv.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 {
		t.Fatalf("expected exactly 1 tool message, got %d", len(toolMsgs))
	}
	if !strings.Contains(toolMsgs[0].Content, "tool panicked: critical slice out of bounds") {
		t.Errorf("expected panic converted to tool error, got: %q", toolMsgs[0].Content)
	}

	last := conv.Messages[len(conv.Messages)-1]
	if last.Role != "assistant" || last.Content != "done" {
		t.Errorf("expected conversation to continue to final turn after recovery, got: role=%q content=%q", last.Role, last.Content)
	}
}

// mockAgentServer returns an SSE server that replies with the given chunk
// sequence in openai-completions streaming format.
func mockAgentServer(t *testing.T, chunks []string, requests *[][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests != nil {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("failed to read request body: %v", readErr)
			}
			*requests = append(*requests, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
}

// TestAgentRunSingleTurn verifies the full loop: user input -> streamed reply
// -> usage footer, with no tool calls.
func TestAgentRunSingleTurn(t *testing.T) {
	server := mockAgentServer(t, []string{
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello"}`,
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":" world"}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}, nil)
	defer server.Close()

	client := chat.NewClient(mockAgentConfig(server.URL))
	runner := agent.NewRunner(client, mockAgentConfig(server.URL))

	conv := &session.Conversation{ID: "c1", Name: "test", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}
	err := runner.Run(context.Background(), conv, "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	roles := []string{}
	for _, m := range conv.Messages {
		roles = append(roles, m.Role)
	}
	if len(roles) != 3 || roles[0] != "system" || roles[1] != "user" || roles[2] != "assistant" {
		t.Errorf("unexpected message sequence: %v", roles)
	}
	if conv.Messages[2].Content != "Hello world" {
		t.Errorf("unexpected assistant content: %q", conv.Messages[2].Content)
	}
}

// TestAgentRunToolCallLoop verifies the multi-step loop: model requests a tool,
// the runner executes it, feeds the output back, and the model finishes.
func TestAgentRunToolCallLoop(t *testing.T) {
	step := 0
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}

	var chunksA = []string{
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"list_files"}}`,
		`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_1","delta":"{}"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"list_files","arguments":"{}"}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}
	var chunksB = []string{
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"done"}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}

	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-mu
		step++
		current := chunksA
		if step > 1 {
			current = chunksB
		}
		mu <- struct{}{}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range current {
			fmt.Fprintf(w, "%s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := mockAgentConfig(server.URL)
	client := chat.NewClient(cfg)
	runner := agent.NewRunner(client, cfg)

	conv := &session.Conversation{ID: "c1", Name: "test", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}
	if err := runner.Run(context.Background(), conv, "list files"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := map[string]bool{}
	for _, m := range conv.Messages {
		found[m.Role] = true
	}
	if !found["tool"] {
		t.Error("expected a tool message in the conversation after tool call")
	}
	if !found["assistant"] {
		t.Error("expected a final assistant message after the tool result")
	}
}

// TestAgentRunHalt verifies context cancellation stops the loop cleanly.
func TestAgentRunHalt(t *testing.T) {
	server := mockAgentServer(t, []string{
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		`data: [DONE]`,
	}, nil)
	defer server.Close()

	cfg := mockAgentConfig(server.URL)
	client := chat.NewClient(cfg)
	runner := agent.NewRunner(client, cfg)

	conv := &session.Conversation{ID: "c1", Name: "test", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := runner.Run(ctx, conv, "hi"); err != nil {
		t.Errorf("expected clean halt without error, got: %v", err)
	}
}

// TestAgentRunnerCustomTools verifies a runner built with a specific tool set
// only exposes those tools.
func TestAgentRunnerCustomTools(t *testing.T) {
	cfg := mockAgentConfig("http://127.0.0.1:1") // unreachable; not dialed
	client := chat.NewClient(cfg)
	runner := agent.NewRunnerWithTools(client, cfg, []tools.Tool{&tools.ReadFileTool{}})

	if len(runner.ToolDefs) != 1 || runner.ToolDefs[0].Function.Name != "read_file" {
		t.Errorf("expected exactly read_file tool, got %d tools", len(runner.ToolDefs))
	}
	if _, ok := runner.Tools["read_file"]; !ok {
		t.Error("expected read_file in tools map")
	}
}

// TestAgentRunToolExecutionError verifies a failing tool surfaces its error to
// the model as a tool message instead of aborting the loop.
func TestAgentRunToolExecutionError(t *testing.T) {
	var chunksA = []string{
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"list_files"}}`,
		`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_1","delta":"not-json"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"list_files","arguments":"not-json"}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}
	var chunksB = []string{
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"recovered"}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}

	step := 0
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		current := chunksA
		if step > 1 {
			current = chunksB
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range current {
			fmt.Fprintf(w, "%s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := mockAgentConfig(server.URL)
	client := chat.NewClient(cfg)
	runner := agent.NewRunner(client, cfg)

	conv := &session.Conversation{ID: "c1", Name: "test", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}
	if err := runner.Run(context.Background(), conv, "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var toolMsg *chat.Message
	for i := range conv.Messages {
		if conv.Messages[i].Role == "tool" {
			toolMsg = &conv.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("expected tool error message to be fed back to the model")
	}
	if !strings.Contains(toolMsg.Content, "error") && !strings.Contains(toolMsg.Content, "invalid") {
		t.Errorf("expected tool error content, got: %q", toolMsg.Content)
	}
}

func TestHandleHistoryEmpty(t *testing.T) {
	conv := &session.Conversation{ID: "empty"}
	reader := bufio.NewReader(strings.NewReader(""))
	if err := agent.HandleHistory(conv, reader); err != nil {
		t.Fatalf("expected nil error on empty history, got: %v", err)
	}
}

func TestShortcutsTable(t *testing.T) {
	tbl := agent.ShortcutsTable()
	if len(tbl.Headers) != 3 {
		t.Fatalf("expected 3 headers, got %d", len(tbl.Headers))
	}
	rendered := tbl.Render()
	if !strings.Contains(rendered, "Ctrl+N") || !strings.Contains(rendered, "Ctrl+Y") || !strings.Contains(rendered, "/shortcuts") {
		t.Fatalf("expected rendered table to contain shortcuts, got:\n%s", rendered)
	}
}

func TestHandleResumeDirect(t *testing.T) {
	sm := session.LoadSessionManager("", config.ModelRef{Provider: "p", Model: "m"})

	// Empty conversations
	sm.Conversations = make(map[string]*session.Conversation)
	reader := bufio.NewReader(strings.NewReader(""))
	if err := agent.HandleResume(config.Config{}, sm, reader); err != nil {
		t.Fatalf("unexpected error on empty resume: %v", err)
	}

	// Populated conversations - user confirms index 1
	c1 := sm.New("c1")
	c1.Messages = append(c1.Messages, chat.Message{Role: "user", Content: "Hello from test"})
	if err := sm.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	reader = bufio.NewReader(strings.NewReader("1\n"))
	if err := agent.HandleResume(config.Config{}, sm, reader); err != nil {
		t.Fatalf("unexpected error on populated resume: %v", err)
	}

	// Unconfirmed / empty choice
	reader = bufio.NewReader(strings.NewReader("\n"))
	if err := agent.HandleResume(config.Config{}, sm, reader); err != nil {
		t.Fatalf("unexpected error on unconfirmed resume: %v", err)
	}
}

func TestComprehensiveAgentToolRendering(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testFile := "sample.txt"
	if err := os.WriteFile(testFile, []byte("line 1: initial\nline 2: hello\nline 3: world\n"), 0644); err != nil {
		t.Fatal(err)
	}

	conv := &session.Conversation{
		ID:   "render-test",
		Name: "render-test",
		Messages: []chat.Message{
			{Role: "user", Content: "do some edits"},
			{
				Role: "assistant",
				ToolCalls: []chat.ToolCall{
					{
						ID: "c1",
						Function: chat.ToolCallFunction{
							Name:      "read_file",
							Arguments: fmt.Sprintf(`{"path": "%s", "offset": 1}`, testFile),
						},
					},
					{
						ID: "c2",
						Function: chat.ToolCallFunction{
							Name:      "write_file",
							Arguments: `{"path": "created.txt", "content": "hello new file"}`,
						},
					},
					{
						ID: "c3",
						Function: chat.ToolCallFunction{
							Name:      "edit_file",
							Arguments: fmt.Sprintf(`{"path": "%s", "old_string": "hello", "new_string": "goodbye"}`, testFile),
						},
					},
					{
						ID: "c4",
						Function: chat.ToolCallFunction{
							Name:      "list_files",
							Arguments: `{"path": "."}`,
						},
					},
					{
						ID: "c5",
						Function: chat.ToolCallFunction{
							Name:      "run_command",
							Arguments: `{"command": "echo test"}`,
						},
					},
					{
						ID: "c6",
						Function: chat.ToolCallFunction{
							Name:      "web_search",
							Arguments: `{"query": "golang"}`,
						},
					},
					{
						ID: "c7",
						Function: chat.ToolCallFunction{
							Name:      "fetch_url",
							Arguments: `{"url": "https://example.com"}`,
						},
					},
				},
			},
			{Role: "tool", ToolCallID: "c1", Content: "[empty file]"},
			{Role: "tool", ToolCallID: "c2", Content: "successfully wrote 14 bytes to created.txt"},
			{Role: "tool", ToolCallID: "c3", Content: "error: failed to edit"},
		},
	}

	// Exercise PrintConversationHistory with all tool calls and roles
	agent.PrintConversationHistory(conv)
}

func TestComprehensiveHandleHistoryActions(t *testing.T) {
	conv := &session.Conversation{
		ID:   "hist-test",
		Name: "hist-test",
		Messages: []chat.Message{
			{Role: "user", Content: "What is Go?"},
			{Role: "assistant", Content: "Go is an open source programming language."},
		},
	}

	// In this piped test environment there is no clipboard channel (no TTY for
	// OSC 52, no wl-copy/xclip/xsel), so HandleHistory must surface the
	// explanatory clipboard-unavailable error instead of a false success.
	expectCopyOutcome := func(action string, err error) {
		t.Helper()
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "clipboard unavailable") {
			t.Fatalf("HandleHistory %s failed: %v", action, err)
		}
	}

	// 1. Choose turn 1, then choose copy AI output (selection 1)
	reader := bufio.NewReader(strings.NewReader("1\n1\n"))
	expectCopyOutcome("copy output", agent.HandleHistory(conv, reader))

	// 2. Choose turn 1, then choose copy prompt (selection 2)
	reader = bufio.NewReader(strings.NewReader("1\n2\n"))
	expectCopyOutcome("copy prompt", agent.HandleHistory(conv, reader))

	// 3. Choose turn 1, then choose copy both markdown (selection 3)
	reader = bufio.NewReader(strings.NewReader("1\n3\n"))
	expectCopyOutcome("copy both", agent.HandleHistory(conv, reader))

	// 4. Choose turn 1, then view (selection 4)
	reader = bufio.NewReader(strings.NewReader("1\n4\n"))
	if err := agent.HandleHistory(conv, reader); err != nil {
		t.Fatalf("HandleHistory view failed: %v", err)
	}
}

func TestComprehensiveHandleResumeCallbacks(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"p": {BaseURL: "http://localhost", Models: []config.Model{{ID: "m"}}},
		},
	}
	modelRef := config.ModelRef{Provider: "p", Model: "m"}
	sm := session.LoadSessionManager("", modelRef)
	sm.New("conv-to-rename")

	// Test selecting and resuming conversation
	reader := bufio.NewReader(strings.NewReader("1\n"))
	if err := agent.HandleResume(cfg, sm, reader); err != nil {
		t.Fatalf("HandleResume failed: %v", err)
	}
}

// TestParentContextCancellationAgent verifies that cancelling the parent
// context immediately terminates an active agent turn (including SSE streaming)
// without hanging.
func TestParentContextCancellationAgent(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"start\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Block indefinitely until server or gate is closed
		select {
		case <-gate:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: server.URL,
				API:     "responses",
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	runner := agent.NewRunner(client, cfg)
	conv := &session.Conversation{
		ID:    "test-cancel-conv",
		Name:  "cancel test",
		Model: config.ModelRef{Provider: "mock", Model: "mock-model"},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, conv, "test prompt")
	}()

	// Allow the request to establish connection
	time.Sleep(50 * time.Millisecond)

	// Cancel the parent context
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected clean exit on cancellation, got err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent failed to terminate within 2s after parent context was cancelled")
	}
}

// TestConcurrentAgentStress tests multiple independent agent instances concurrently
// under load and race detector to verify shared infrastructure resilience.
func TestConcurrentAgentStress(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\n")
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: server.URL,
				API:     "responses",
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}

	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := chat.NewClient(cfg)
			runner := agent.NewRunner(client, cfg)
			conv := &session.Conversation{
				ID:    fmt.Sprintf("conv-stress-%d", id),
				Name:  "stress test",
				Model: config.ModelRef{Provider: "mock", Model: "mock-model"},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			_ = runner.Run(ctx, conv, fmt.Sprintf("prompt %d", id))
		}(i)
	}
	wg.Wait()
}

func TestEnsureTrailingNewline(t *testing.T) {
	if got := agent.EnsureTrailingNewline(""); got != "" {
		t.Errorf("empty stays empty, got %q", got)
	}
	if got := agent.EnsureTrailingNewline("hello\n"); got != "hello\n" {
		t.Errorf("already terminated unchanged, got %q", got)
	}
	if got := agent.EnsureTrailingNewline("hello"); got != "hello\n" {
		t.Errorf("missing newline appended, got %q", got)
	}
}

func TestHandleResumeRenameDeleteCancel(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"p": {BaseURL: "http://localhost", Models: []config.Model{{ID: "m"}}},
		},
	}
	newSM := func() *session.SessionManager {
		sm := session.LoadSessionManager("", config.ModelRef{Provider: "p", Model: "m"})
		sm.New("target")
		return sm
	}

	// Ctrl+E rename flow: select item 1, press Ctrl+E ('e'), type new name
	sm := newSM()
	reader := bufio.NewReader(strings.NewReader("1\nrename-to-x\n"))
	if err := agent.HandleResume(cfg, sm, reader); err != nil {
		t.Fatalf("HandleResume rename flow failed: %v", err)
	}

	// Ctrl+D delete flow: select item 1, press Ctrl+D, confirm with y
	sm = newSM()
	sm.New("keepme")
	reader = bufio.NewReader(strings.NewReader("1\ny\n"))
	if err := agent.HandleResume(cfg, sm, reader); err != nil {
		t.Fatalf("HandleResume delete flow failed: %v", err)
	}

	// Esc cancel: send escape sequence
	sm = newSM()
	reader = bufio.NewReader(strings.NewReader("\x1b\n"))
	if err := agent.HandleResume(cfg, sm, reader); err != nil {
		t.Fatalf("HandleResume cancel flow failed: %v", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() {
		os.Stdout = old
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("failed to close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("failed to read captured stdout: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("failed to close pipe reader: %v", err)
	}
	return buf.String()
}

func TestModelRetentionAcrossNew(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"test-provider": {
				BaseURL: "http://localhost:1234/v1",
				Models: []config.Model{
					{ID: "model-1"},
					{ID: "model-2"},
					{ID: "model-3"},
				},
			},
		},
	}

	defaultRef := config.ModelRef{Provider: "test-provider", Model: "model-1"}
	sm := session.LoadSessionManager("", defaultRef)

	if sm.Active().Model.Model != "model-1" {
		t.Fatalf("expected initial model-1, got %s", sm.Active().Model.Model)
	}

	// 1. Switch to model-2 via HandleModels
	reader := bufio.NewReader(strings.NewReader("2\n"))
	if err := agent.HandleModels(cfg, sm, reader); err != nil {
		t.Fatalf("HandleModels failed: %v", err)
	}
	if sm.Active().Model.Model != "model-2" {
		t.Fatalf("expected active model to switch to model-2, got %s", sm.Active().Model.Model)
	}

	// 2. Create new chat (Ctrl+N / HandleNew) -> should keep model-2, NOT revert to model-1!
	if err := agent.HandleNew(sm); err != nil {
		t.Fatalf("HandleNew failed: %v", err)
	}
	if sm.Active().Model.Model != "model-2" {
		t.Fatalf("HandleNew reset model to %s instead of retaining model-2", sm.Active().Model.Model)
	}

	// 3. Switch to model-3 via HandleModels
	reader = bufio.NewReader(strings.NewReader("3\n"))
	if err := agent.HandleModels(cfg, sm, reader); err != nil {
		t.Fatalf("HandleModels failed: %v", err)
	}
	if sm.Active().Model.Model != "model-3" {
		t.Fatalf("expected active model to switch to model-3, got %s", sm.Active().Model.Model)
	}

	// 4. Create new chat (Ctrl+N / HandleNew) -> should keep model-3!
	if err := agent.HandleNew(sm); err != nil {
		t.Fatalf("HandleNew failed: %v", err)
	}
	if sm.Active().Model.Model != "model-3" {
		t.Fatalf("HandleNew reset model to %s instead of retaining model-3", sm.Active().Model.Model)
	}
}

func TestModelRetentionAcrossProgramRestart(t *testing.T) {
	tempDir := t.TempDir()
	sessionFile := tempDir

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"test-provider": {
				BaseURL: "http://localhost:1234/v1",
				Models: []config.Model{
					{ID: "model-1"},
					{ID: "model-2"},
				},
			},
		},
	}

	model1 := config.ModelRef{Provider: "test-provider", Model: "model-1"}
	model2 := config.ModelRef{Provider: "test-provider", Model: "model-2"}

	// 1. Initial process: start hmj with model-1 as config default
	sm1 := session.LoadSessionManager(sessionFile, model1)
	if sm1.Active().Model.Model != "model-1" {
		t.Fatalf("expected initial model-1, got %s", sm1.Active().Model.Model)
	}

	// 2. User switches to model-2 via HandleModels
	reader := bufio.NewReader(strings.NewReader("2\n"))
	if err := agent.HandleModels(cfg, sm1, reader); err != nil {
		t.Fatalf("HandleModels failed: %v", err)
	}
	if sm1.Active().Model != model2 {
		t.Fatalf("expected model-2, got %s", sm1.Active().Model.Model)
	}

	// 3. User closes agent with Ctrl+D (triggers saveSession)
	if err := sm1.Save(); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if err := sm1.Close(); err != nil {
		t.Fatal(err)
	}

	// 4. User reopens by typing hmj (new process loads session with model-1 config default)
	sm2 := session.LoadSessionManager(sessionFile, model1)
	active := sm2.Active()
	if active.Model.Model != "model-2" {
		t.Fatalf("reopened active conversation has %s, expected persisted model-2", active.Model.Model)
	}

	// 5. User presses Ctrl+N (/new) after reopening
	cNew := sm2.New("")
	if cNew.Model.Model != "model-2" {
		t.Fatalf("new conversation after reopen has %s, expected model-2", cNew.Model.Model)
	}
}

func TestModelFallbackWhenModelRemovedFromConfig(t *testing.T) {
	tempDir := t.TempDir()
	sessionFile := tempDir

	// Initial config has model-1 and model-deprecated
	cfgInitial := config.Config{
		Providers: map[string]config.Provider{
			"test-provider": {
				BaseURL: "http://localhost:1234/v1",
				Models: []config.Model{
					{ID: "model-1"},
					{ID: "model-deprecated"},
				},
			},
		},
	}

	model1, err := cfgInitial.DefaultModel()
	if err != nil {
		t.Fatalf("DefaultModel failed: %v", err)
	}
	sm1 := session.LoadSessionManager(sessionFile, model1)

	// User switched to model-deprecated
	reader := bufio.NewReader(strings.NewReader("2\n"))
	if err := agent.HandleModels(cfgInitial, sm1, reader); err != nil {
		t.Fatalf("HandleModels failed: %v", err)
	}
	if sm1.Active().Model.Model != "model-deprecated" {
		t.Fatalf("expected model-deprecated, got %s", sm1.Active().Model.Model)
	}
	if err := sm1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if err := sm1.Close(); err != nil {
		t.Fatal(err)
	}

	// Now user modifies config.json to remove model-deprecated before restarting hmj:
	cfgUpdated := config.Config{
		Providers: map[string]config.Provider{
			"test-provider": {
				BaseURL: "http://localhost:1234/v1",
				Models: []config.Model{
					{ID: "model-1"},
				},
			},
		},
	}

	modelDefault, err := cfgUpdated.DefaultModel()
	if err != nil {
		t.Fatalf("DefaultModel failed: %v", err)
	}
	sm2 := session.LoadSessionManager(sessionFile, modelDefault)
	conv := sm2.Active()

	// Simulate main.go startup validation
	if _, err := cfgUpdated.Resolve(conv.Model); err != nil {
		conv.Model = modelDefault
		sm2.SetDefaultModel(conv.Model)
	}

	if conv.Model.Model != "model-1" {
		t.Fatalf("expected fallback to model-1 when model-deprecated is missing, got %s", conv.Model.Model)
	}
	if sm2.DefaultModel.Model != "model-1" {
		t.Fatalf("expected sm2.DefaultModel fallback to model-1, got %s", sm2.DefaultModel.Model)
	}
}

func TestAutoRefreshModelsOnConfigUpdate(t *testing.T) {
	tempDir := t.TempDir()
	configPath := tempDir + "/config.json"

	// 1. Initial config on disk with model-1 only
	initialJSON := `{
		"providers": {
			"my-provider": {
				"baseUrl": "https://api.example.com/v1",
				"models": [
					{"id": "model-1", "name": "Model One"}
				]
			}
		}
	}`
	if err := os.WriteFile(configPath, []byte(initialJSON), 0600); err != nil {
		t.Fatalf("failed writing initial config: %v", err)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed loading initial config: %v", err)
	}
	if len(cfg.AllModels()) != 1 {
		t.Fatalf("expected 1 initial model, got %d", len(cfg.AllModels()))
	}

	sm := session.LoadSessionManager("", config.ModelRef{Provider: "my-provider", Model: "model-1"})

	// 2. User modifies config.json while agent is running to add model-2 and model-3
	updatedJSON := `{
		"providers": {
			"my-provider": {
				"baseUrl": "https://api.example.com/v1",
				"models": [
					{"id": "model-1", "name": "Model One"},
					{"id": "model-2", "name": "Model Two"},
					{"id": "model-3", "name": "Model Three"}
				]
			}
		}
	}`
	if err := os.WriteFile(configPath, []byte(updatedJSON), 0600); err != nil {
		t.Fatalf("failed writing updated config: %v", err)
	}

	// 3. User triggers Ctrl+O / models selection -> auto-reload fires
	reloadedCfg, err := config.ReloadConfig(configPath, cfg)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if len(reloadedCfg.AllModels()) != 3 {
		t.Fatalf("expected 3 reloaded models, got %d", len(reloadedCfg.AllModels()))
	}

	// 4. User selects dynamically added model-3 (item 3)
	reader := bufio.NewReader(strings.NewReader("3\n"))
	if err := agent.HandleModels(reloadedCfg, sm, reader); err != nil {
		t.Fatalf("HandleModels failed with reloaded config: %v", err)
	}

	if sm.Active().Model.Model != "model-3" {
		t.Fatalf("expected active model to switch to model-3, got %s", sm.Active().Model.Model)
	}

	// 5. If user saves malformed JSON (e.g. while editing), reload fails gracefully keeping previous models
	malformedJSON := `{"providers": {`
	if err := os.WriteFile(configPath, []byte(malformedJSON), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	safeCfg, err := config.ReloadConfig(configPath, reloadedCfg)
	if err == nil {
		t.Fatalf("expected error for malformed config")
	}
	if len(safeCfg.AllModels()) != 3 {
		t.Fatalf("expected previous 3 models to be preserved on malformed config, got %d", len(safeCfg.AllModels()))
	}
}

// TestAgentHelpers verifies small pure helpers used by the run loop.
func TestAgentHelpers(t *testing.T) {
	if got := agent.FormatNumber(1500000); got != "1,500,000" {
		t.Errorf("FormatNumber(1500000) = %q, want 1,500,000", got)
	}
	if got := agent.FormatNumber(950); got != "950" {
		t.Errorf("FormatNumber(950) = %q, want 950", got)
	}
	if got := agent.FormatDuration(90 * time.Second); got != "1m30s" {
		t.Errorf("FormatDuration(90s) = %q, want 1m30s", got)
	}
}

func TestSessionManagerAndCommands(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"dummy": {
				BaseURL: "http://localhost:1234/v1",
				Models:  []config.Model{{ID: "model-a"}, {ID: "model-b"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	runner := agent.NewRunner(client, cfg)

	if len(runner.Tools) != 9 {
		t.Fatalf("expected 9 tools, got: %d", len(runner.Tools))
	}

	modelRef := config.ModelRef{Provider: "dummy", Model: "model-a"}
	sm := session.LoadSessionManager("", modelRef)

	if sm.Active().Name != "default" {
		t.Fatalf("expected default conversation, got %s", sm.Active().Name)
	}

	c2 := sm.New("feature-auth")
	if sm.Active().ID != c2.ID || sm.Active().Name != "feature-auth" {
		t.Fatalf("expected active conversation feature-auth, got %s", sm.Active().Name)
	}

	resumed, err := sm.Resume("1")
	if err != nil || resumed.Name != "default" {
		t.Fatalf("failed resuming conversation 1: %v", err)
	}

	// Test shortcut action handler: HandleNew adds a conversation and keeps the earlier ones
	if err := agent.HandleNew(sm); err != nil {
		t.Fatalf("HandleNew failed: %v", err)
	}
	if len(sm.List()) != 3 {
		t.Fatalf("expected 3 conversations after HandleNew, got %d", len(sm.List()))
	}

	agent.PrintConversationHistory(sm.Active())
}

func TestInteractiveCommandsAndShortcuts(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"dummy": {
				BaseURL: "http://localhost:1234/v1",
				Models:  []config.Model{{ID: "model-a"}, {ID: "model-b"}},
			},
		},
	}
	modelRef := config.ModelRef{Provider: "dummy", Model: "model-a"}
	sm := session.LoadSessionManager("", modelRef)
	sm.New("second-chat")

	// 1. Interactive HandleModels selection (user types "2")
	reader := bufio.NewReader(strings.NewReader("2\n"))
	err := agent.HandleModels(cfg, sm, reader)
	if err != nil {
		t.Fatalf("HandleModels interactive failed: %v", err)
	}
	if sm.Active().Model.Model != "model-b" {
		t.Fatalf("expected active model to switch to model-b, got %s", sm.Active().Model.Model)
	}

	// 2. Interactive HandleResume selection by 1-based index (user types "1" to switch back to default)
	reader = bufio.NewReader(strings.NewReader("1\n"))
	err = agent.HandleResume(cfg, sm, reader)
	if err != nil {
		t.Fatalf("HandleResume interactive failed: %v", err)
	}
	if sm.Active().Name != "default" {
		t.Fatalf("expected active to be default, got %s", sm.Active().Name)
	}

	// 3. Interactive HandleResume cancellation on empty Enter
	reader = bufio.NewReader(strings.NewReader("\n"))
	err = agent.HandleResume(cfg, sm, reader)
	if err != nil {
		t.Fatalf("HandleResume cancel failed: %v", err)
	}
	if sm.Active().Name != "default" {
		t.Fatalf("expected active to remain default, got %s", sm.Active().Name)
	}
}

// TestAgentRunWritesDiagnosticLog verifies that a tool call and each completed model
// step are recorded in the diagnostic log opened by logging.Init.
func TestAgentRunWritesDiagnosticLog(t *testing.T) {
	toolCall := []string{
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"list_files"}}`,
		`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_1","delta":"{}"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"list_files","arguments":"{}"}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}
	answer := []string{
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"done"}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}

	t.Chdir(t.TempDir())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunks := answer
		if calls.Add(1) == 1 {
			chunks = toolCall
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	logPath := filepath.Join(t.TempDir(), "hmj.log")
	closeLog, err := logging.Init(logPath)
	if err != nil {
		t.Fatalf("logging.Init: %v", err)
	}
	t.Cleanup(func() {
		if _, err := logging.Init(""); err != nil {
			t.Errorf("reset logger: %v", err)
		}
	})

	cfg := mockAgentConfig(server.URL)
	runner := agent.NewRunner(chat.NewClient(cfg), cfg)
	conv := &session.Conversation{ID: "c1", Name: "test", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}
	if err := runner.Run(context.Background(), conv, "list files"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := closeLog(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var toolRec, stepRec map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid JSON log line %q: %v", line, err)
		}
		switch rec["msg"] {
		case "tool":
			toolRec = rec
		case "llm step":
			stepRec = rec
		}
	}
	if toolRec == nil {
		t.Fatalf("no tool record in log:\n%s", data)
	}
	if toolRec["name"] != "list_files" {
		t.Errorf("tool record name = %v, want list_files", toolRec["name"])
	}
	if _, ok := toolRec["error"]; ok {
		t.Errorf("successful tool record should omit error: %v", toolRec)
	}
	if stepRec == nil || stepRec["provider"] != "mock" {
		t.Errorf("expected an llm step record for provider mock, got %v", stepRec)
	}
}

// responsesStep builds one streamed model step in the responses wire format: an optional
// function call or text, followed by a completion event carrying token usage.
func responsesStep(toolName, callID, args, text string, in, out int) []string {
	var chunks []string
	if toolName != "" {
		chunks = append(chunks,
			fmt.Sprintf("event: response.output_item.added\ndata: %s", mustJSON(map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"id": "item_" + callID, "type": "function_call", "call_id": callID, "name": toolName},
			})),
			fmt.Sprintf("event: response.function_call_arguments.delta\ndata: %s", mustJSON(map[string]any{
				"type": "response.function_call_arguments.delta", "call_id": callID, "delta": args,
			})),
			fmt.Sprintf("event: response.output_item.done\ndata: %s", mustJSON(map[string]any{
				"type": "response.output_item.done", "output_index": 0,
				"item": map[string]any{"id": "item_" + callID, "type": "function_call", "call_id": callID, "name": toolName, "arguments": args},
			})),
		)
	}
	if text != "" {
		chunks = append(chunks, fmt.Sprintf("event: response.output_text.delta\ndata: %s",
			mustJSON(map[string]any{"type": "response.output_text.delta", "delta": text})))
	}
	return append(chunks, fmt.Sprintf("event: response.completed\ndata: %s", mustJSON(map[string]any{
		"type": "response.completed",
		"response": map[string]any{"status": "completed", "usage": map[string]any{
			"input_tokens": in, "output_tokens": out, "total_tokens": in + out,
		}},
	})))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestAgentRunMultiTurnWriteThenReadBack drives a full tool conversation through a real
// session: the model writes a file, reads it back, and answers. It checks the file on
// disk, the transcript, what the model was sent at each step, the persisted session and
// the token totals shown in the turn footer.
func TestAgentRunMultiTurnWriteThenReadBack(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	steps := [][]string{
		responsesStep("write_file", "call_w", `{"path":"notes/out.txt","content":"hello from the model\n"}`, "", 100, 10),
		responsesStep("read_file", "call_r", `{"path":"notes/out.txt"}`, "", 150, 20),
		responsesStep("", "", "", "The file says hello.", 200, 30),
	}
	var (
		mu       sync.Mutex
		requests []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(body), "Reply with the title only") {
			// The title request that follows the first turn is not a step of the conversation.
			for _, chunk := range responsesStep("", "", "", "Note file round trip", 20, 5) {
				fmt.Fprintf(w, "%s\n\n", chunk)
			}
			return
		}
		mu.Lock()
		n := len(requests)
		requests = append(requests, string(body))
		mu.Unlock()
		if n >= len(steps) {
			t.Errorf("unexpected request %d", n+1)
			http.Error(w, "too many requests", http.StatusInternalServerError)
			return
		}
		for _, chunk := range steps[n] {
			fmt.Fprintf(w, "%s\n\n", chunk)
		}
	}))
	defer server.Close()

	cfg := mockAgentConfig(server.URL)
	sessionPath := t.TempDir()
	sm := session.LoadSessionManager(sessionPath, config.ModelRef{Provider: "mock", Model: "mock-model"})
	conv := sm.Active()
	runner := agent.NewRunner(chat.NewClient(cfg), cfg)

	out := captureStdout(t, func() {
		if err := runner.Run(context.Background(), conv, "write a note then read it back"); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	// The file the model asked for exists with the requested content.
	data, err := os.ReadFile(filepath.Join(dir, "notes", "out.txt"))
	if err != nil || string(data) != "hello from the model\n" {
		t.Fatalf("file on disk = %q, %v", data, err)
	}

	// Transcript: system, user, assistant(write), tool, assistant(read), tool, assistant(final).
	roles := make([]string, len(conv.Messages))
	for i, m := range conv.Messages {
		roles[i] = m.Role
	}
	if want := "system user assistant tool assistant tool assistant"; strings.Join(roles, " ") != want {
		t.Fatalf("roles = %v, want %s", roles, want)
	}
	m := conv.Messages
	if m[2].ToolCalls[0].Function.Name != "write_file" || m[2].ToolCalls[0].ID != "call_w" {
		t.Errorf("first tool call = %+v", m[2].ToolCalls)
	}
	if m[3].ToolCallID != "call_w" || m[3].Content == "" {
		t.Errorf("write result = %+v", m[3])
	}
	if m[4].ToolCalls[0].Function.Name != "read_file" || m[5].ToolCallID != "call_r" || !strings.Contains(m[5].Content, "hello from the model") {
		t.Errorf("read step = call %+v, result %q", m[4].ToolCalls, m[5].Content)
	}
	if m[6].Content != "The file says hello." {
		t.Errorf("final answer = %q", m[6].Content)
	}
	if conv.Name != "Note file round trip" {
		t.Errorf("conversation name = %q, want the generated title", conv.Name)
	}

	// Each step re-sent the growing history; the last request carried the file content back.
	if len(requests) != 3 {
		t.Fatalf("model called %d times, want 3", len(requests))
	}
	if !strings.Contains(requests[0], "write a note then read it back") || strings.Contains(requests[0], "call_w") {
		t.Errorf("first request should hold only the prompt: %.200s", requests[0])
	}
	if !strings.Contains(requests[1], "call_w") || strings.Contains(requests[1], "call_r") {
		t.Errorf("second request should include the write result only: %.300s", requests[1])
	}
	if !strings.Contains(requests[2], "call_r") || !strings.Contains(requests[2], "hello from the model") {
		t.Errorf("third request should include the file content: %.300s", requests[2])
	}

	// Usage footer: last prompt size, summed completion and total tokens, step count.
	for _, want := range []string{"200 input", "60 output", "510 total"} {
		if !strings.Contains(out, want) {
			t.Errorf("footer missing %q in output:\n%s", want, out)
		}
	}
	if !strings.Contains(theme.StripANSI(out), "3 steps") {
		t.Errorf("footer missing step count:\n%s", out)
	}

	// The session persists the whole exchange.
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	if err := sm.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded := session.LoadSessionManager(sessionPath, config.ModelRef{})
	if got := reloaded.Active().Messages; len(got) != 7 || got[6].Content != "The file says hello." {
		t.Errorf("reloaded session has %d messages", len(got))
	}
}

// A turn is never compacted while it runs, however large the conversation. Once it has ended
// over the limit, the conversation is compacted with the provider's compact endpoint, and the next
// turn carries the compacted window in place of the history.
func TestCompactionRunsAfterTheTurnOverTheLimit(t *testing.T) {
	t.Chdir(t.TempDir())
	var mu sync.Mutex
	var paths, requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		requests = append(requests, string(body))
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/compact") {
			fmt.Fprint(w, `{"output":[{"type":"compaction","encrypted_content":"ENCRYPTED-WINDOW"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":450000,\"output_tokens\":5,\"total_tokens\":450005}}}\n\n")
	}))
	defer server.Close()

	cfg := mockAgentConfig(server.URL)
	runner := agent.NewRunner(chat.NewClient(cfg), cfg)
	conv := &session.Conversation{ID: "c1", Model: config.ModelRef{Provider: "mock", Model: "mock-model"}}
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "system", Content: "SYSTEM"},
		chat.Message{Role: "user", Content: "old request"},
		chat.Message{Role: "assistant", Content: "old reply"})

	if err := runner.Run(context.Background(), conv, "new request"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("got %d requests during the turn, want only the turn's own", len(requests))
	}

	runner.StartCompaction(context.Background(), conv).Finish()
	if len(requests) != 2 || paths[1] != "/responses/compact" {
		t.Fatalf("requests to %v, want the turn and one compact request", paths)
	}
	if req := requests[1]; !strings.Contains(req, "old request") || !strings.Contains(req, "new request") {
		t.Error("the compact request should carry the whole conversation")
	}
	if n := len(conv.Messages); n != 2 || conv.Messages[1].CompactionBlock() == nil {
		t.Fatalf("after compaction the conversation is %+v, want the system prompt and the compaction", conv.Messages)
	}

	if err := runner.Run(context.Background(), conv, "next request"); err != nil {
		t.Fatal(err)
	}
	body := requests[2]
	if !strings.Contains(body, "next request") || !strings.Contains(body, "ENCRYPTED-WINDOW") || strings.Contains(body, "old request") {
		t.Error("the next turn should carry the compacted window and the new prompt, not the compacted history")
	}
}
