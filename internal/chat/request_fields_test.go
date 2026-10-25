package chat

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hmj-agent/internal/config"
)

// capturedBody serves a fixed SSE body and returns a pointer to the request body it received.
func capturedBody(t *testing.T, sse string) (url string, body *string) {
	t.Helper()
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	t.Cleanup(server.Close)
	return server.URL, &got
}

// TestResponsesRequestReasoningFields checks the reasoning, storage and output-limit fields of a
// Responses API request, and that effort is sent only when configured.
func TestResponsesRequestReasoningFields(t *testing.T) {
	url, body := capturedBody(t, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: url, API: "responses", Models: []config.Model{{ID: "m", Effort: "high", MaxOutputTokens: 1234}}},
	}}
	if _, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"store":false`,
		`"include":["reasoning.encrypted_content"]`,
		`"reasoning":{"effort":"high","summary":"auto"}`,
		`"max_output_tokens":1234`,
	} {
		if !strings.Contains(*body, want) {
			t.Errorf("request missing %s: %s", want, *body)
		}
	}

	url2, body2 := capturedBody(t, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	cfg2 := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url2, API: "responses", Models: []config.Model{{ID: "m"}}}}}
	if _, _, err := NewClient(cfg2).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*body2, `"reasoning":{"summary":"auto"}`) || strings.Contains(*body2, `"effort"`) || strings.Contains(*body2, "max_output_tokens") {
		t.Errorf("unset model request = %s, want summary only", *body2)
	}
}

// TestResponsesToolsAreFlat checks that a Responses API request gives each function tool its
// name, description and parameters beside its type, as the API requires (a tool nested under
// "function", the Chat Completions shape, is refused with "Missing required parameter:
// 'tools[0].name'"), and keeps the tools non-strict.
func TestResponsesToolsAreFlat(t *testing.T) {
	url, body := capturedBody(t, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	cfg := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url, API: "responses", Models: []config.Model{{ID: "m"}}}}}
	tools := []ToolDefinition{{Type: "function", Function: FunctionDefinition{Name: "read_file", Description: "Reads a file.", Parameters: Schema{"type": "object"}}}}
	if _, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, tools, nil, nil); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(*body), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %v, want one", req.Tools)
	}
	tool := req.Tools[0]
	if tool["type"] != "function" || tool["name"] != "read_file" || tool["description"] != "Reads a file." || tool["strict"] != false || tool["parameters"] == nil {
		t.Errorf("tool = %v, want type, name, description, parameters and strict false at the top level", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Errorf("tool = %v, must not nest its fields under function", tool)
	}
}

// TestResponsesReasoningRoundTrip checks that a streamed reasoning item (with its summary and
// encrypted content) is kept and replayed before the function_call items of the same turn.
func TestResponsesReasoningRoundTrip(t *testing.T) {
	sse := `event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"ENC-1","summary":[{"type":"summary_text","text":"Planning the read"}]}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","delta":"Planning"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file"}}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","call_id":"call_1","arguments":"{\"path\":\"a.go\"}"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`
	url, _ := capturedBody(t, sse)
	cfg := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url, API: "responses", Models: []config.Model{{ID: "m"}}}}}
	var reasoning strings.Builder
	msg, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, func(s string) { reasoning.WriteString(s) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reasoning.String() != "Planning" {
		t.Errorf("streamed reasoning = %q, want the summary delta", reasoning.String())
	}
	want := Block{Type: "reasoning", Text: "Planning the read", Signature: "rs_1", Data: "ENC-1"}
	if len(msg.Blocks) != 1 || msg.Blocks[0] != want {
		t.Fatalf("blocks = %+v, want %+v", msg.Blocks, want)
	}

	items := convertMessagesToInputItems([]Message{{Role: "user", Content: "x"}, msg})
	if len(items) != 3 {
		t.Fatalf("input items = %+v, want user, reasoning, function_call", items)
	}
	if items[1].Type != "reasoning" || items[1].ID != "rs_1" || items[1].EncryptedContent != "ENC-1" {
		t.Errorf("replayed reasoning item = %+v", items[1])
	}
	if items[2].Type != "function_call" {
		t.Errorf("item after reasoning = %+v, want function_call", items[2])
	}
	raw, err := json.Marshal(items[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"summary":[]`) {
		t.Errorf("reasoning item must send an empty summary: %s", raw)
	}
}

// TestResponsesMessagePhasesRoundTrip checks that message items the model labelled with a phase
// are kept with it and sent back as separate items, phase and all, while the message's Content
// holds all its text; a reply without phases goes back as one message, as before.
func TestResponsesMessagePhasesRoundTrip(t *testing.T) {
	sse := `event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"Reading the file."}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Reading the file."}]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file"}}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{}"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`
	url, _ := capturedBody(t, sse)
	cfg := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url, API: "responses", Models: []config.Model{{ID: "m"}}}}}
	msg, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Reading the file." {
		t.Errorf("content = %q", msg.Content)
	}
	items := convertMessagesToInputItems([]Message{msg})
	if len(items) != 2 || items[0].Type != "message" || items[0].Phase != "commentary" || items[0].Content != "Reading the file." || items[1].Type != "function_call" {
		t.Fatalf("replayed items = %+v, want the commentary message then the call", items)
	}
	raw, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"phase":"commentary"`) {
		t.Errorf("message item = %s, want its phase", raw)
	}

	plain := convertMessagesToInputItems([]Message{{Role: "assistant", Content: "Done."}})
	if len(plain) != 1 || plain[0].Content != "Done." || plain[0].Phase != "" {
		t.Errorf("unlabelled reply = %+v, want one message without a phase", plain)
	}
}

// TestCompletionsRequestMaxTokens checks that max_tokens is sent only when the model sets it.
func TestCompletionsRequestMaxTokens(t *testing.T) {
	url, body := capturedBody(t, "data: [DONE]\n\n")
	cfg := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url, API: "completions", Models: []config.Model{{ID: "m", MaxOutputTokens: 77}}}}}
	if _, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*body, `"max_tokens":77`) {
		t.Errorf("request = %s, want max_tokens 77", *body)
	}

	url2, body2 := capturedBody(t, "data: [DONE]\n\n")
	cfg2 := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url2, API: "completions", Models: []config.Model{{ID: "m"}}}}}
	if _, _, err := NewClient(cfg2).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*body2, "max_tokens") {
		t.Errorf("unset model request = %s, want no max_tokens", *body2)
	}
}

// TestCompletionsRequestReasoningEffort checks that a Chat Completions request sends the model's
// effort as reasoning_effort, and not when thinking is off or no effort is set.
func TestCompletionsRequestReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		model config.Model
		want  string
	}{
		{config.Model{ID: "m", Effort: "minimal"}, `"reasoning_effort":"minimal"`},
		{config.Model{ID: "m", Effort: "high", Thinking: "off"}, ""},
		{config.Model{ID: "m"}, ""},
	} {
		url, body := capturedBody(t, "data: [DONE]\n\n")
		cfg := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url, API: "completions", Models: []config.Model{tc.model}}}}
		if _, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if tc.want == "" && strings.Contains(*body, "reasoning_effort") || tc.want != "" && !strings.Contains(*body, tc.want) {
			t.Errorf("effort %q thinking %q: request = %s, want %q", tc.model.Effort, tc.model.Thinking, *body, cmp.Or(tc.want, "no reasoning_effort"))
		}
	}
}

// TestAnthropicEffortMapsOpenAILevels checks that OpenAI's none and minimal go to Anthropic as
// low, and its own levels as they are.
func TestAnthropicEffortMapsOpenAILevels(t *testing.T) {
	for effort, want := range map[string]string{"none": "low", "minimal": "low", "low": "low", "max": "max"} {
		raw, err := json.Marshal(anthropicRequestFor(config.Model{ID: "m", Effort: effort}, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"output_config":{"effort":"`+want+`"}`) {
			t.Errorf("effort %s: request = %s, want %s", effort, raw, want)
		}
	}
}

// TestGeminiGenerationConfig checks the output limit, thought summaries and thinking level of the
// Interactions generation_config.
func TestGeminiGenerationConfig(t *testing.T) {
	tests := []struct {
		name   string
		model  config.Model
		want   string
		absent string
	}{
		{"low effort", config.Model{ID: "m", Effort: "low", MaxOutputTokens: 500}, `"generation_config":{"max_output_tokens":500,"thinking_level":"low","thinking_summaries":"auto"}`, ""},
		{"medium effort", config.Model{ID: "m", Effort: "medium"}, `"generation_config":{"thinking_level":"medium","thinking_summaries":"auto"}`, "max_output_tokens"},
		{"other effort maps to high", config.Model{ID: "m", Effort: "max"}, `"generation_config":{"thinking_level":"high","thinking_summaries":"auto"}`, "max_output_tokens"},
		{"none maps to minimal", config.Model{ID: "m", Effort: "none"}, `"generation_config":{"thinking_level":"minimal","thinking_summaries":"auto"}`, ""},
		{"unset", config.Model{ID: "m"}, `"generation_config":{"thinking_summaries":"auto"}`, "thinking_level"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, body := capturedBody(t, geminiCompletedSSE)
			cfg := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url, API: "gemini", Models: []config.Model{tc.model}}}}
			_, _, _ = NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil)
			if !strings.Contains(*body, tc.want) {
				t.Errorf("request = %s, want %s", *body, tc.want)
			}
			if tc.absent != "" && strings.Contains(*body, tc.absent) {
				t.Errorf("request = %s, must not contain %s", *body, tc.absent)
			}
		})
	}
}

// TestProbeHeadersAnthropicSendsKeyOnce checks that a health probe authenticates Anthropic with
// x-api-key alone, without a duplicate Authorization header.
func TestProbeHeadersAnthropicSendsKeyOnce(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	setProbeHeaders(req, config.APIAnthropic, "k")
	if req.Header.Get("x-api-key") != "k" || req.Header.Get("Authorization") != "" {
		t.Errorf("headers = %v, want x-api-key only", req.Header)
	}
}

// TestThinkingOffOmitsReasoningFields checks that a model with thinking "off" sends no reasoning
// parameters on any protocol, and that the default still sends them. Compatible endpoints and
// non-reasoning models reject these fields.
func TestThinkingOffOmitsReasoningFields(t *testing.T) {
	t.Run("responses", func(t *testing.T) {
		sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
		send := func(thinking string) string {
			url, body := capturedBody(t, sse)
			cfg := config.Config{Providers: map[string]config.Provider{
				"p": {BaseURL: url, API: "responses", Models: []config.Model{{ID: "m", Thinking: thinking}}},
			}}
			// A reasoning item in history must not be replayed when thinking is off.
			history := []Message{
				{Role: "user", Content: "x"},
				{Role: "assistant", Blocks: []Block{{Type: "reasoning", Signature: "rs_1", Data: "ENC"}}, Content: "answer"},
			}
			if _, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, history, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			return *body
		}
		on := send("")
		for _, want := range []string{`"store":false`, `"include":["reasoning.encrypted_content"]`, `"reasoning":{"summary":"auto"}`, `"encrypted_content":"ENC"`} {
			if !strings.Contains(on, want) {
				t.Errorf("default request missing %s: %s", want, on)
			}
		}
		off := send("off")
		if !strings.Contains(off, `"store":false`) {
			t.Errorf("thinking off: request must still not be stored: %s", off)
		}
		for _, unwanted := range []string{`"include"`, `"reasoning"`, `"encrypted_content"`} {
			if strings.Contains(off, unwanted) {
				t.Errorf("thinking off: request must not contain %s: %s", unwanted, off)
			}
		}
		if !strings.Contains(off, `"content":"answer"`) {
			t.Errorf("thinking off must keep the message text: %s", off)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		on, err := json.Marshal(anthropicRequestFor(config.Model{ID: "m"}, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		off, err := json.Marshal(anthropicRequestFor(config.Model{ID: "m", Thinking: "off"}, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(on), `"thinking":{"type":"adaptive","display":"summarized"}`) {
			t.Errorf("default request missing thinking: %s", on)
		}
		if strings.Contains(string(off), `"thinking"`) || strings.Contains(string(off), `"disabled"`) {
			t.Errorf("thinking off must omit the field entirely: %s", off)
		}
	})

	t.Run("gemini", func(t *testing.T) {
		send := func(m config.Model) string {
			url, body := capturedBody(t, geminiCompletedSSE)
			cfg := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: url, API: "gemini", Models: []config.Model{m}}}}
			_, _, _ = NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil)
			return *body
		}
		off := send(config.Model{ID: "m", Thinking: "off", MaxOutputTokens: 900})
		if strings.Contains(off, "thinking_level") || strings.Contains(off, "thinking_summaries") {
			t.Errorf("thinking off: request must not contain thinking fields: %s", off)
		}
		if !strings.Contains(off, `"generation_config":{"max_output_tokens":900}`) {
			t.Errorf("thinking off must keep max_output_tokens: %s", off)
		}
		if on := send(config.Model{ID: "m"}); !strings.Contains(on, `"thinking_summaries":"auto"`) {
			t.Errorf("default request missing thought summaries: %s", on)
		}
	})
}
