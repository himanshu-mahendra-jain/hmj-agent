package chat

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hmj-agent/v2/internal/config"
)

const geminiTestKey = "test-gemini-key"

// geminiCompletedSSE is a minimal stream that ends with a completed interaction.
const geminiCompletedSSE = "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"status\":\"completed\"}}\n\n"

// newGeminiTestClient serves handler behind an httptest server and returns a
// client plus the provider and model pointing at it. The base URL has the
// /v1beta suffix that hmj configs use.
func newGeminiTestClient(t *testing.T, handler http.HandlerFunc) (*Client, config.Provider, config.Model) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	provider := config.Provider{
		BaseURL: server.URL + "/v1beta",
		APIKey:  geminiTestKey,
		API:     config.APIGemini,
		Models:  []config.Model{{ID: "gemini-test"}},
	}
	cfg := config.Config{Providers: map[string]config.Provider{"gem": provider}}
	return NewClient(cfg), provider, config.Model{ID: "gemini-test"}
}

// writeGeminiSSE writes each event as one "data:" SSE line. The event_type inside
// each JSON object is what the parser reads, as in the live API.
func writeGeminiSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, ev := range events {
		fmt.Fprintf(w, "data: %s\n\n", ev)
	}
	fmt.Fprint(w, "event: done\ndata: [DONE]\n\n")
}

// requireGeminiAuth records a test failure unless the request is the expected
// streaming call: POST to /v1beta/interactions with the key in x-goog-api-key.
func requireGeminiAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("expected POST, got %s", r.Method)
	}
	if r.URL.Path != "/v1beta/interactions" {
		t.Errorf("unexpected path %q", r.URL.Path)
	}
	if got := r.Header.Get("x-goog-api-key"); got != geminiTestKey {
		t.Errorf("expected x-goog-api-key %q, got %q", geminiTestKey, got)
	}
	if got := r.Header.Get("Authorization"); got != "" {
		t.Errorf("gemini must not send Authorization, got %q", got)
	}
}

func TestGeminiStreamPlainText(t *testing.T) {
	client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireGeminiAuth(t, r)
		writeGeminiSSE(w,
			`{"event_type":"interaction.created","interaction":{"status":"in_progress"}}`,
			`{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`,
			`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"Hel"}}`,
			`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"lo"}}`,
			`{"event_type":"step.stop","index":0}`,
			`{"event_type":"interaction.completed","interaction":{"status":"completed","usage":{"total_input_tokens":10,"total_output_tokens":5,"total_thought_tokens":0,"total_cached_tokens":4,"total_tokens":15}}}`,
		)
	})

	var streamed []string
	msg, usage, _, _, err := client.streamGeminiOnce(context.Background(), provider, model,
		[]Message{{Role: "user", Content: "hi"}}, nil, nil, func(s string) { streamed = append(streamed, s) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Role != "assistant" || msg.Content != "Hello" {
		t.Fatalf("unexpected message: %+v", msg)
	}
	if strings.Join(streamed, "|") != "Hel|lo" {
		t.Fatalf("expected deltas Hel|lo, got %q", streamed)
	}
	if usage.PromptTokens != 10 || usage.InputTokens != 10 {
		t.Fatalf("unexpected prompt tokens: %+v", usage)
	}
	if usage.CompletionTokens != 5 || usage.OutputTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("unexpected completion/total tokens: %+v", usage)
	}
	if usage.CachedTokens != 4 || usage.EffectiveCachedTokens() != 4 {
		t.Fatalf("unexpected cached tokens: %+v", usage)
	}
	if usage.FinishReason != "stop" {
		t.Fatalf("expected finish reason stop, got %q", usage.FinishReason)
	}
	if usage.Duration <= 0 {
		t.Fatalf("expected duration to be recorded, got %v", usage.Duration)
	}
}

func TestGeminiStreamThoughtGoesToReasoningAndBlocks(t *testing.T) {
	client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireGeminiAuth(t, r)
		writeGeminiSSE(w,
			`{"event_type":"step.start","index":0,"step":{"type":"thought"}}`,
			`{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":"pondering"}}}`,
			`{"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"sig-t"}}`,
			`{"event_type":"step.stop","index":0}`,
			`{"event_type":"step.start","index":1,"step":{"type":"model_output"}}`,
			`{"event_type":"step.delta","index":1,"delta":{"type":"text","text":"answer"}}`,
			`{"event_type":"step.stop","index":1}`,
			`{"event_type":"interaction.completed","interaction":{"status":"completed","usage":{"total_input_tokens":3,"total_output_tokens":2,"total_thought_tokens":7,"total_tokens":12}}}`,
		)
	})

	var reasoning, content []string
	msg, usage, _, _, err := client.streamGeminiOnce(context.Background(), provider, model,
		[]Message{{Role: "user", Content: "hi"}}, nil,
		func(s string) { reasoning = append(reasoning, s) },
		func(s string) { content = append(content, s) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(reasoning, "") != "pondering" {
		t.Fatalf("expected reasoning 'pondering', got %q", reasoning)
	}
	if strings.Join(content, "") != "answer" || msg.Content != "answer" {
		t.Fatalf("thought text leaked into content: deltas=%q msg=%q", content, msg.Content)
	}
	// The thought step is kept with its signature so it can be replayed unchanged.
	if len(msg.Blocks) != 1 || msg.Blocks[0] != (Block{Type: "thought", Text: "pondering", Signature: "sig-t"}) {
		t.Fatalf("expected one thought block with its signature, got %+v", msg.Blocks)
	}
	// Thought tokens are billed as output, so they count toward completion.
	if usage.CompletionTokens != 9 || usage.TotalTokens != 12 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestGeminiStreamFunctionCall(t *testing.T) {
	client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireGeminiAuth(t, r)
		writeGeminiSSE(w,
			`{"event_type":"step.start","index":0,"step":{"id":"fc-1","type":"function_call","name":"read_file","arguments":{},"signature":"sig-abc"}}`,
			`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"path\":"}}`,
			`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"\"a\"}"}}`,
			`{"event_type":"step.stop","index":0}`,
			`{"event_type":"step.start","index":1,"step":{"id":"fc-9","type":"function_call","name":"list_dir","arguments":{}}}`,
			`{"event_type":"step.stop","index":1}`,
			`{"event_type":"interaction.completed","interaction":{"status":"requires_action","usage":{"total_input_tokens":4,"total_output_tokens":6,"total_tokens":10}}}`,
		)
	})

	msg, usage, _, _, err := client.streamGeminiOnce(context.Background(), provider, model,
		[]Message{{Role: "user", Content: "go"}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(msg.ToolCalls))
	}

	first := msg.ToolCalls[0]
	if first.ID != "fc-1" || first.Type != "function" || first.Function.Name != "read_file" {
		t.Fatalf("unexpected first call: %+v", first)
	}
	if first.Function.Arguments != `{"path":"a"}` {
		t.Fatalf("argument deltas not accumulated, got %q", first.Function.Arguments)
	}
	if first.ThoughtSignature != "sig-abc" {
		t.Fatalf("expected function-call signature preserved, got %q", first.ThoughtSignature)
	}

	second := msg.ToolCalls[1]
	if second.ID != "fc-9" || second.Function.Name != "list_dir" {
		t.Fatalf("unexpected second call: %+v", second)
	}
	if second.Function.Arguments != "{}" {
		t.Fatalf("expected {} for empty arguments, got %q", second.Function.Arguments)
	}
	if second.ThoughtSignature != "" {
		t.Fatalf("unexpected signature on second call: %q", second.ThoughtSignature)
	}
	if usage.FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls for requires_action, got %q", usage.FinishReason)
	}
}

func TestGeminiMissingCallIDIsMintedDistinctAcrossResponses(t *testing.T) {
	a := newGeminiAccumulator(time.Now())
	b := newGeminiAccumulator(time.Now())
	for _, acc := range []*geminiAccumulator{a, b} {
		if err := acc.handle("", `{"event_type":"step.start","index":0,"step":{"type":"function_call","name":"f","arguments":{}}}`, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	msgA, _ := a.result()
	msgB, _ := b.result()
	idA, idB := msgA.ToolCalls[0].ID, msgB.ToolCalls[0].ID
	if !strings.HasPrefix(idA, geminiGeneratedCallIDPrefix) {
		t.Fatalf("expected a generated id, got %q", idA)
	}
	if idA == idB {
		t.Fatalf("generated ids collide across responses: %q", idA)
	}
}

func TestConvertMessagesToGemini(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys A"},
		{Role: "system", Content: "sys B"},
		{Role: "user", Content: "hi"},
		{
			Role:   "assistant",
			Blocks: []Block{{Type: "thought", Text: "plan", Signature: "sig-th"}, {Type: "thinking", Text: "other provider"}},
			ToolCalls: []ToolCall{
				{
					ID:               "fc-1",
					Type:             "function",
					Function:         ToolCallFunction{Name: "read_file", Arguments: `{"path":"a"}`},
					ThoughtSignature: "sig-1",
				},
				{
					ID:       "fc-2",
					Type:     "function",
					Function: ToolCallFunction{Name: "grep", Arguments: `{"q":"x"}`},
				},
			},
		},
		{Role: "tool", ToolCallID: "fc-1", Content: "file body"},
		{Role: "tool", ToolCallID: "fc-2", Content: "matches"},
		{Role: "user", Content: "next question"},
	}

	system, input := convertMessagesToGemini(msgs)
	if system != "sys A\n\nsys B" {
		t.Fatalf("unexpected system instruction: %q", system)
	}
	if len(input) != 7 {
		t.Fatalf("expected 7 steps (user, thought, call, call, 2 results, user), got %d: %+v", len(input), input)
	}
	if input[0].Type != "user_input" || input[0].Content[0].Text != "hi" {
		t.Fatalf("unexpected first step: %+v", input[0])
	}

	thought := input[1]
	if thought.Type != "thought" || thought.Signature != "sig-th" || thought.Summary[0].Text != "plan" {
		t.Fatalf("thought block must be replayed with its signature and summary, got %+v", thought)
	}
	call := input[2]
	if call.Type != "function_call" || call.ID != "fc-1" || call.Name != "read_file" || call.Arguments["path"] != "a" {
		t.Fatalf("unexpected function_call step: %+v", call)
	}
	if call.Signature != "sig-1" {
		t.Fatalf("signature must be echoed on the same function_call step, got %q", call.Signature)
	}
	call2 := input[3]
	if call2.ID != "fc-2" || call2.Name != "grep" || call2.Signature != "" {
		t.Fatalf("unexpected second function_call (no signature should be invented): %+v", call2)
	}

	r1 := input[4]
	if r1.Type != "function_result" || r1.CallID != "fc-1" || r1.Name != "read_file" || r1.Result[0].Text != "file body" {
		t.Fatalf("unexpected first function_result: %+v", r1)
	}
	r2 := input[5]
	if r2.CallID != "fc-2" || r2.Name != "grep" || r2.Result[0].Text != "matches" {
		t.Fatalf("unexpected second function_result: %+v", r2)
	}
	if input[6].Type != "user_input" || input[6].Content[0].Text != "next question" {
		t.Fatalf("expected follow-up user input last, got %+v", input[6])
	}
}

func TestConvertMessagesToGeminiDummySignature(t *testing.T) {
	// History from another provider (no signatures, minted IDs) gets the documented
	// dummy signature on the first call only. Empty messages emit no steps.
	msgs := []Message{
		{Role: "user", Content: ""},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "gemini_call_old_1", Function: ToolCallFunction{Name: "a", Arguments: ""}},
				{ID: "gemini_call_old_2", Function: ToolCallFunction{Name: "b", Arguments: "not json"}},
			},
		},
		{Role: "tool", ToolCallID: "gemini_call_old_1", Content: "r1"},
		{Role: "tool", ToolCallID: "gemini_call_old_2", Content: "r2"},
		{Role: "assistant", Content: ""},
	}

	_, input := convertMessagesToGemini(msgs)
	if len(input) != 4 {
		t.Fatalf("expected 2 calls and 2 results only, got %d: %+v", len(input), input)
	}
	if input[0].Signature != geminiDummySignature {
		t.Fatalf("expected dummy signature on first call, got %q", input[0].Signature)
	}
	if input[1].Signature != "" {
		t.Fatalf("dummy signature must only go on the first call, got %q", input[1].Signature)
	}
	if args := input[1].Arguments; args["_malformed_arguments"] != "not json" {
		t.Fatalf("malformed arguments must be preserved, got %+v", args)
	}
	if input[2].Name != "a" {
		t.Fatalf("expected result name looked up from the earlier call, got %+v", input[2])
	}
}

func TestGeminiRequestWireFormat(t *testing.T) {
	captured := make(chan []byte, 1)
	client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireGeminiAuth(t, r)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		captured <- raw
		writeGeminiSSE(w, `{"event_type":"interaction.completed","interaction":{"status":"completed"}}`)
	})

	tools := []ToolDefinition{{
		Type: "function",
		Function: FunctionDefinition{
			Name:        "read_file",
			Description: "reads a file",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		},
	}}
	msgs := []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "read a"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "fc-1", Function: ToolCallFunction{Name: "read_file", Arguments: `{"path":"a"}`}, ThoughtSignature: "sig"}}},
		{Role: "tool", ToolCallID: "fc-1", Content: "hello"},
	}
	model.MaxOutputTokens = 700
	model.Effort = "low"

	if _, _, _, _, err := client.streamGeminiOnce(context.Background(), provider, model, msgs, tools, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(<-captured, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if body["model"] != "gemini-test" || body["store"] != false || body["stream"] != true {
		t.Fatalf("expected model, store:false and stream:true, got %+v", body)
	}
	if _, ok := body["previous_interaction_id"]; ok {
		t.Fatal("stateless requests must not send previous_interaction_id")
	}
	if body["system_instruction"] != "be brief" {
		t.Fatalf("system instruction not sent as a string: %+v", body["system_instruction"])
	}

	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "read_file" || tool["description"] != "reads a file" {
		t.Fatalf("unexpected tool declaration: %+v", tool)
	}
	if _, ok := tool["parameters"]; !ok {
		t.Fatalf("expected JSON Schema under parameters, got %+v", tool)
	}
	if _, ok := tool["function"]; ok {
		t.Fatal("tool must be flat, not wrapped in a function object")
	}

	gc := body["generation_config"].(map[string]any)
	if gc["max_output_tokens"] != float64(700) || gc["thinking_level"] != "low" || gc["thinking_summaries"] != "auto" {
		t.Fatalf("unexpected generation_config: %+v", gc)
	}

	input := body["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("expected user_input, function_call and function_result steps, got %d: %+v", len(input), input)
	}
	if step := input[0].(map[string]any); step["type"] != "user_input" {
		t.Fatalf("expected user_input first, got %+v", step)
	}
	call := input[1].(map[string]any)
	if call["type"] != "function_call" || call["id"] != "fc-1" || call["name"] != "read_file" || call["signature"] != "sig" {
		t.Fatalf("unexpected function_call step: %+v", call)
	}
	if args := call["arguments"].(map[string]any); args["path"] != "a" {
		t.Fatalf("arguments must be a JSON object, got %+v", args)
	}
	result := input[2].(map[string]any)
	if result["type"] != "function_result" || result["call_id"] != "fc-1" || result["name"] != "read_file" {
		t.Fatalf("unexpected function_result step: %+v", result)
	}
	if text := result["result"].([]any)[0].(map[string]any); text["type"] != "text" || text["text"] != "hello" {
		t.Fatalf("function result must be a text content list, got %+v", text)
	}
}

func TestGeminiRequestOmitsEmptyFields(t *testing.T) {
	raw, err := json.Marshal(geminiRequest{
		Model: "m",
		Input: []geminiStep{{Type: "user_input", Content: []geminiText{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, absent := range []string{"system_instruction", "tools", "generation_config", "arguments", "signature", "call_id"} {
		if strings.Contains(s, absent) {
			t.Fatalf("expected %q to be omitted, got %s", absent, s)
		}
	}
	if !strings.Contains(s, `"store":false`) {
		t.Fatalf("store must always be sent as false, got %s", s)
	}
	if convertToolsToGemini(nil) != nil {
		t.Fatal("expected nil tools for empty tool list")
	}
}

func TestGeminiInteractionsURL(t *testing.T) {
	tests := []struct {
		base, want string
	}{
		{"https://generativelanguage.googleapis.com/v1beta", "https://generativelanguage.googleapis.com/v1beta/interactions"},
		{"https://example.test/v1beta/", "https://example.test/v1beta/interactions"},
		{"https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com/v1beta/interactions"},
		{"https://example.test/v1", "https://example.test/v1beta/interactions"},
		{"https://example.test/v1beta/interactions", "https://example.test/v1beta/interactions"},
	}
	for _, tt := range tests {
		if got := geminiInteractionsURL(tt.base); got != tt.want {
			t.Errorf("geminiInteractionsURL(%q) = %q, want %q", tt.base, got, tt.want)
		}
	}
}

func TestGeminiThinkingLevel(t *testing.T) {
	tests := map[string]string{"": "", "low": "low", "medium": "medium", "high": "high", "xhigh": "high", "max": "high"}
	for effort, want := range tests {
		if got := geminiThinkingLevel(effort); got != want {
			t.Errorf("geminiThinkingLevel(%q) = %q, want %q", effort, got, want)
		}
	}
}

func TestGeminiMaxTokensMapsToLength(t *testing.T) {
	// An interaction that hits its output limit reports status "incomplete".
	client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireGeminiAuth(t, r)
		writeGeminiSSE(w,
			`{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`,
			`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"cut off"}}`,
			`{"event_type":"interaction.completed","interaction":{"status":"incomplete"}}`,
		)
	})

	msg, usage, _, _, err := client.streamGeminiOnce(context.Background(), provider, model,
		[]Message{{Role: "user", Content: "long"}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Content != "cut off" {
		t.Fatalf("unexpected content %q", msg.Content)
	}
	if usage.FinishReason != "length" {
		t.Fatalf("expected finish reason length, got %q", usage.FinishReason)
	}
}

func TestGeminiFinishReasonMapping(t *testing.T) {
	tests := map[string]string{
		"completed":       "stop",
		"incomplete":      "length",
		"requires_action": "tool_calls",
		"cancelled":       "cancelled",
	}
	for status, want := range tests {
		if got := geminiFinishReason(status); got != want {
			t.Errorf("geminiFinishReason(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestGeminiFailedInteractionIsError(t *testing.T) {
	client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireGeminiAuth(t, r)
		writeGeminiSSE(w,
			`{"event_type":"interaction.completed","interaction":{"status":"failed","errors":[{"code":"internal","message":"model gave up"}]}}`,
		)
	})

	_, _, _, _, err := client.streamGeminiOnce(context.Background(), provider, model,
		[]Message{{Role: "user", Content: "x"}}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "model gave up") {
		t.Fatalf("expected failed interaction error, got %v", err)
	}
}

// TestGeminiHTTPErrorClassification checks the retry decision for error bodies in
// the Interactions shape {"error":{"code":...,"message":...}}. Rate limits and
// 5xx are retried, a 429 hint is honored, and 4xx other than 429 fails fast.
func TestGeminiHTTPErrorClassification(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryable  bool
		retryAfter time.Duration
	}{
		{"rate limited with retry hint", http.StatusTooManyRequests,
			`{"error":{"message":"Resource has been exhausted (e.g. check quota). Please retry in 39s.","code":"resource_exhausted"}}`,
			true, 39 * time.Second},
		{"unavailable", http.StatusServiceUnavailable,
			`{"error":{"message":"The model is overloaded. Please try again later.","code":"unavailable"}}`,
			true, 0},
		{"internal", http.StatusInternalServerError,
			`{"error":{"message":"internal error","code":"internal"}}`,
			true, 0},
		{"bad api key", http.StatusForbidden,
			`{"error":{"message":"API key not valid. Please pass a valid API key.","code":"permission_denied"}}`,
			false, 0},
		{"unknown model", http.StatusNotFound,
			`{"error":{"message":"Model 'no-such-model' not found.","code":"not_found"}}`,
			false, 0},
		{"bad request", http.StatusBadRequest,
			`{"error":{"message":"When using the steps-based API version, use step_list input format instead of turn_list.","code":"invalid_request"}}`,
			false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requireGeminiAuth(t, r)
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			})

			_, _, retryAfter, retryable, err := client.streamGeminiOnce(context.Background(), provider, model,
				[]Message{{Role: "user", Content: "x"}}, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tt.status)) {
				t.Fatalf("expected HTTP %d error, got %v", tt.status, err)
			}
			if retryable != tt.retryable {
				t.Fatalf("retryable = %v, want %v", retryable, tt.retryable)
			}
			if retryAfter != tt.retryAfter {
				t.Fatalf("retry-after = %v, want %v", retryAfter, tt.retryAfter)
			}
		})
	}
}

// TestGeminiHTTPErrorIsShownAsCodeAndMessage checks that an error body, also one framed as a
// server-sent error event as some 400s come, is shown as its code and message, not raw JSON.
func TestGeminiHTTPErrorIsShownAsCodeAndMessage(t *testing.T) {
	const want = "HTTP 400: invalid_request: Request contains an invalid argument."
	for name, body := range map[string]string{
		"json": `{"error":{"message":"Request contains an invalid argument.","code":"invalid_request"}}`,
		"sse": "event: error\ndata: {\"error\":{\"message\":\"Request contains an invalid argument.\"," +
			"\"code\":\"invalid_request\"},\"event_type\":\"error\"}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, body)
			})
			_, _, _, _, err := client.streamGeminiOnce(context.Background(), provider, model,
				[]Message{{Role: "user", Content: "x"}}, nil, nil, nil)
			if err == nil || err.Error() != want {
				t.Errorf("err = %v, want %q", err, want)
			}
		})
	}
}

func TestGeminiInStreamErrorIsReturned(t *testing.T) {
	client, provider, model := newGeminiTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireGeminiAuth(t, r)
		writeGeminiSSE(w,
			`{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`,
			`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"partial"}}`,
			`{"event_type":"error","error":{"code":"internal","message":"backend exploded"}}`,
		)
	})

	_, _, _, _, err := client.streamGeminiOnce(context.Background(), provider, model,
		[]Message{{Role: "user", Content: "x"}}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "backend exploded") {
		t.Fatalf("expected in-stream error, got %v", err)
	}
}

func TestGeminiInvalidEventJSONIsError(t *testing.T) {
	acc := newGeminiAccumulator(time.Now())
	if err := acc.handle("", "not json", nil, nil); err == nil {
		t.Fatal("expected error for non-JSON stream data")
	}
	if err := acc.handle("", "  ", nil, nil); err != nil {
		t.Fatalf("blank data should be ignored, got %v", err)
	}
	if err := acc.handle("done", "[DONE]", nil, nil); err != nil {
		t.Fatalf("[DONE] should be ignored, got %v", err)
	}
}

func TestToolCallThoughtSignatureRoundTrip(t *testing.T) {
	in := ToolCall{
		ID:               "c1",
		Type:             "function",
		Function:         ToolCallFunction{Name: "f", Arguments: "{}"},
		ThoughtSignature: "sig-xyz",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ToolCall
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip mismatch: got %+v, want %+v", out, in)
	}

	// An unsigned call must not add the field to persisted sessions.
	unsigned, err := json.Marshal(ToolCall{ID: "c2", Type: "function"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unsigned), "thought_signature") {
		t.Fatalf("empty signature should be omitted, got %s", unsigned)
	}
}

func TestProbeModelGemini(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Any other path 404s, so a healthy result also proves the endpoint.
		if r.URL.Path != "/v1beta/interactions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("x-goog-api-key") != geminiTestKey || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"max_output_tokens":1`) || !strings.Contains(string(raw), `"store":false`) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"completed","object":"interaction"}`)
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"gem": {BaseURL: server.URL + "/v1beta", APIKey: geminiTestKey, API: config.APIGemini, Models: []config.Model{{ID: "gemini-test"}}},
	}}
	res := NewClient(cfg).ProbeModel(context.Background(), config.ModelRef{Provider: "gem", Model: "gemini-test"})
	if res.Status != StatusHealthy {
		t.Fatalf("expected healthy probe, got %s (%s)", res.Status, res.Details)
	}

	// Interactions error bodies use {"error":{"message":...}}; the probe must surface it.
	errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"API key not valid. Please pass a valid API key.","code":"invalid_request"}}`)
	}))
	defer errServer.Close()
	cfg.Providers["gem"] = config.Provider{BaseURL: errServer.URL + "/v1beta", APIKey: geminiTestKey, API: config.APIGemini, Models: []config.Model{{ID: "gemini-test"}}}
	res = NewClient(cfg).ProbeModel(context.Background(), config.ModelRef{Provider: "gem", Model: "gemini-test"})
	if res.Status != StatusFailed || !strings.Contains(res.Details, "API key not valid") {
		t.Fatalf("expected failed probe with Gemini error message, got %s (%s)", res.Status, res.Details)
	}
}
