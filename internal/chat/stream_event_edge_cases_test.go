package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hmj-agent/internal/config"
)

// longGarbage is non-JSON stream data longer than the 200-character snippet an error quotes.
var longGarbage = "<" + strings.Repeat("é", 300) + ">"

// TestInvalidEventJSONQuotesShortSnippet checks, for each protocol's stream parser, that data
// that is not JSON is an error and that the error quotes only a bounded, rune-safe snippet.
func TestInvalidEventJSONQuotesShortSnippet(t *testing.T) {
	tests := []struct {
		name   string
		handle func(string) error
		prefix string
	}{
		{"anthropic", func(d string) error { return newAnthropicAccumulator(time.Now()).handle("", d, nil, nil) }, "anthropic stream: invalid event JSON"},
		{"completions", func(d string) error { return newCompletionsAccumulator(time.Now()).handle(d, nil, nil) }, "chat completions stream: invalid chunk JSON"},
		{"gemini", func(d string) error { return newGeminiAccumulator(time.Now()).handle("", d, nil, nil) }, "gemini stream: invalid event JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.handle(longGarbage)
			if err == nil || !strings.HasPrefix(err.Error(), tc.prefix) {
				t.Fatalf("err = %v, want prefix %q", err, tc.prefix)
			}
			if strings.Count(err.Error(), "é") > 200 {
				t.Errorf("error quotes %d characters of the data, want at most 200", strings.Count(err.Error(), "é"))
			}
			if !strings.Contains(err.Error(), "<"+strings.Repeat("é", 50)) {
				t.Errorf("error does not quote the start of the data: %v", err)
			}
		})
	}

	t.Run("responses", func(t *testing.T) {
		if err := newResponsesAccumulator(time.Now()).handle("", "not json", nil, nil); err == nil {
			t.Error("expected an error for non-JSON data")
		}
	})
}

// TestEmptyAndDoneEventsAreIgnored checks that keep-alive blanks and the [DONE] sentinel produce
// neither output nor errors in any protocol.
func TestEmptyAndDoneEventsAreIgnored(t *testing.T) {
	for _, data := range []string{"", "  ", "[DONE]"} {
		a := newAnthropicAccumulator(time.Now())
		g := newGeminiAccumulator(time.Now())
		c := newCompletionsAccumulator(time.Now())
		r := newResponsesAccumulator(time.Now())
		errs := []error{a.handle("ping", data, nil, nil), g.handle("", data, nil, nil), c.handle(data, nil, nil)}
		if data == "[DONE]" {
			errs = append(errs, r.handle("", data, nil, nil))
		}
		for i, err := range errs {
			if err != nil {
				t.Errorf("data %q, parser %d: err = %v", data, i, err)
			}
		}
		if m, _ := a.result(); m.Content != "" {
			t.Errorf("anthropic content = %q", m.Content)
		}
	}
}

// TestAnthropicEventEdgeCases feeds the Anthropic parser events with missing pieces and checks
// what the assistant message and usage end up as.
func TestAnthropicEventEdgeCases(t *testing.T) {
	a := newAnthropicAccumulator(time.Now())
	events := []struct{ typ, data string }{
		// No SSE event line: the type comes from the JSON.
		{"", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`},
		// A tool_use block without an id gets one minted from its index.
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"list_files"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\".\"}"}}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":3}}`},
	}
	for _, ev := range events {
		if err := a.handle(ev.typ, ev.data, nil, nil); err != nil {
			t.Fatalf("handle(%s): %v", ev.data, err)
		}
	}
	msg, usage := a.result()
	if msg.Content != "hi" {
		t.Errorf("content = %q, want hi", msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "toolu_2" || msg.ToolCalls[0].Function.Arguments != `{"path":"."}` {
		t.Errorf("tool calls = %+v, want one toolu_2 call with its arguments", msg.ToolCalls)
	}
	if usage.FinishReason != "length" || usage.CompletionTokens != 3 {
		t.Errorf("usage = finish %q, completion %d; want length, 3", usage.FinishReason, usage.CompletionTokens)
	}
}

// TestAnthropicErrorEventWithoutDetails checks that an error event with no type or message still
// reports the raw event rather than an empty error.
func TestAnthropicErrorEventWithoutDetails(t *testing.T) {
	err := newAnthropicAccumulator(time.Now()).handle("error", `{"type":"error","error":{}}`, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "anthropic stream error") || !strings.Contains(err.Error(), `"type":"error"`) {
		t.Errorf("err = %v, want the raw event quoted", err)
	}
}

// TestToolArgumentsThatAreNotAnObject checks that arguments the model sent which are not a JSON
// object are kept verbatim under _malformed_arguments for both Anthropic and Gemini, instead of
// being dropped.
func TestToolArgumentsThatAreNotAnObject(t *testing.T) {
	tests := []struct {
		args string
		want map[string]any
	}{
		{"", map[string]any{}},
		{"   ", map[string]any{}},
		{`{"a":1}`, map[string]any{"a": float64(1)}},
		{`[1,2]`, map[string]any{"_malformed_arguments": `[1,2]`}},
		{`null`, map[string]any{"_malformed_arguments": `null`}},
		{`"text"`, map[string]any{"_malformed_arguments": `"text"`}},
		{`{"a":`, map[string]any{"_malformed_arguments": `{"a":`}},
	}
	for _, tc := range tests {
		for name, got := range map[string]map[string]any{"anthropic": anthropicToolInput(tc.args), "gemini": geminiToolArgs(tc.args)} {
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("%s(%q) = %v, want %v", name, tc.args, got, tc.want)
			}
		}
	}
}

// TestGeminiEventEdgeCases feeds the Gemini parser events with missing pieces: none may panic or
// produce output, and the SSE event name is used when the data carries no event_type.
func TestGeminiEventEdgeCases(t *testing.T) {
	a := newGeminiAccumulator(time.Now())
	var reasoning []string
	onReasoning := func(s string) { reasoning = append(reasoning, s) }
	events := []struct{ typ, data string }{
		{"", `{"event_type":"interaction.completed"}`},
		{"", `{"event_type":"step.start"}`},
		{"", `{"event_type":"step.delta"}`},
		{"", `{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary"}}`},
		{"", `{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":""}}}`},
		// event_type missing from the data: the SSE event name decides.
		{"step.delta", `{"index":1,"delta":{"type":"text","text":"hello"}}`},
	}
	for _, ev := range events {
		if err := a.handle(ev.typ, ev.data, onReasoning, nil); err != nil {
			t.Fatalf("handle(%s): %v", ev.data, err)
		}
	}
	msg, _ := a.result()
	if msg.Content != "hello" || len(reasoning) != 0 || len(msg.ToolCalls) != 0 {
		t.Errorf("content %q, reasoning %q, calls %v; want hello and nothing else", msg.Content, reasoning, msg.ToolCalls)
	}
}

// TestGeminiConversionFallbacks checks the request-side fallbacks: a tool result whose call was
// compacted away is sent under a placeholder name, assistant text is sent as model output, and a
// tool without a schema is declared with an empty object schema.
func TestGeminiConversionFallbacks(t *testing.T) {
	_, input := convertMessagesToGemini([]Message{
		{Role: "assistant", Content: "Let me look."},
		{Role: "tool", ToolCallID: "gone", Content: "result"},
	})
	if len(input) != 2 || input[0].Type != "model_output" || input[0].Content[0].Text != "Let me look." {
		t.Fatalf("input = %+v, want model_output then function_result", input)
	}
	if input[1].Type != "function_result" || input[1].Name != "unknown_tool" || input[1].CallID != "gone" {
		t.Errorf("function_result = %+v, want name unknown_tool for call gone", input[1])
	}

	tools := convertToolsToGemini([]ToolDefinition{{Type: "function", Function: FunctionDefinition{Name: "noargs"}}})
	if len(tools) != 1 || tools[0].Parameters["type"] != "object" {
		t.Errorf("tools = %+v, want an object schema for a tool without parameters", tools)
	}
}

// TestCompletionsUsageFallbacks checks that usage reported under the Responses-style field names
// is mapped onto the prompt/completion fields, the total is computed when missing, and a tool call
// without an id gets one minted.
func TestCompletionsUsageFallbacks(t *testing.T) {
	a := newCompletionsAccumulator(time.Now())
	chunks := []string{
		`{"choices":[{"delta":{"content":"x","tool_calls":[{"index":0,"function":{"name":"read_file","arguments":"{}"}}]}}]}`,
		`{"choices":[],"usage":{"input_tokens":7,"output_tokens":2,"input_tokens_details":{"cached_tokens":5}}}`,
	}
	for _, c := range chunks {
		if err := a.handle(c, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	msg, usage := a.result()
	if usage.PromptTokens != 7 || usage.CompletionTokens != 2 || usage.TotalTokens != 9 {
		t.Errorf("usage = prompt %d, completion %d, total %d; want 7, 2, 9", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
	if usage.PromptDetails == nil || usage.EffectiveCachedTokens() != 5 {
		t.Errorf("cached tokens = %d, want 5 from input_tokens_details", usage.EffectiveCachedTokens())
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_1" {
		t.Errorf("tool calls = %+v, want one call with minted id call_1", msg.ToolCalls)
	}
}

// TestResponsesCallIDResolution checks how argument events that name no call are attributed: by
// their item ID when it is known, otherwise to the most recent call, and to a new call when there
// is none yet. A name that arrives after the call was created is filled in.
func TestResponsesCallIDResolution(t *testing.T) {
	t.Run("unknown item falls back to the latest call", func(t *testing.T) {
		a := newResponsesAccumulator(time.Now())
		evs := []string{
			`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"c1","name":"a"}}`,
			`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"c2"}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"unknown","delta":"{\"x\":1}"}`,
			`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c2","name":"b"}}`,
		}
		for _, ev := range evs {
			if err := a.handle("", ev, nil, nil); err != nil {
				t.Fatal(err)
			}
		}
		msg, _ := a.result()
		if len(msg.ToolCalls) != 2 {
			t.Fatalf("tool calls = %+v, want 2", msg.ToolCalls)
		}
		if got := msg.ToolCalls[1]; got.ID != "c2" || got.Function.Name != "b" || got.Function.Arguments != `{"x":1}` {
			t.Errorf("second call = %+v, want c2/b with the arguments", got)
		}
		if got := msg.ToolCalls[0]; got.Function.Arguments == `{"x":1}` {
			t.Errorf("first call got the second call's arguments: %+v", got)
		}
	})

	t.Run("no call yet mints one", func(t *testing.T) {
		a := newResponsesAccumulator(time.Now())
		if err := a.handle("response.function_call_arguments.delta", `{"delta":"{}"}`, nil, nil); err != nil {
			t.Fatal(err)
		}
		msg, _ := a.result()
		if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_1" {
			t.Errorf("tool calls = %+v, want one minted call_1", msg.ToolCalls)
		}
	})
}

// TestResponsesSkipsForeignBlocks checks that only Responses reasoning blocks are replayed to a
// Responses endpoint; blocks another protocol produced (Anthropic thinking) are left out.
func TestResponsesSkipsForeignBlocks(t *testing.T) {
	items := convertMessagesToInputItems([]Message{{
		Role:    "assistant",
		Content: "done",
		Blocks:  []Block{{Type: "thinking", Text: "hmm", Signature: "sig"}, {Type: "reasoning", Signature: "rs_1", Data: "enc"}},
	}})
	if len(items) != 2 || items[0].Type != "reasoning" || items[0].ID != "rs_1" || items[1].Type != "message" {
		t.Errorf("items = %+v, want the reasoning item then the message", items)
	}
}

// TestReasoningCountsAsDeliveredOutput checks, for every protocol that streams reasoning, that a
// transient error after reasoning text has been shown is not retried: a retry would stream the
// same reasoning to the user a second time.
func TestReasoningCountsAsDeliveredOutput(t *testing.T) {
	tests := []struct {
		api       string
		reasoning string // one event streaming the reasoning text "thinking"
		failure   string // an in-stream retryable error after it; otherwise the connection drops
	}{
		{config.APIAnthropic,
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thinking\"}}\n\n",
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"},
		{config.APICompletions,
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n",
			""},
		{config.APIResponses,
			"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n",
			""},
		{config.APIGemini,
			"data: {\"event_type\":\"step.delta\",\"index\":0,\"delta\":{\"type\":\"thought_summary\",\"content\":{\"type\":\"text\",\"text\":\"thinking\"}}}\n\n",
			""},
	}
	for _, tc := range tests {
		t.Run(tc.api, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.reasoning)
				if tc.failure != "" {
					fmt.Fprint(w, tc.failure)
					return
				}
				// The connection drops mid-event: a retryable network failure.
				fmt.Fprint(w, "data: {\"partial")
				w.(http.Flusher).Flush()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
			}))
			defer server.Close()

			var reasoning strings.Builder
			_, _, err := clientFor(tc.api, server.URL, 2).StreamChat(context.Background(), testRef, nil, nil,
				func(s string) { reasoning.WriteString(s) }, nil)
			if err == nil {
				t.Fatal("expected the failure to be returned")
			}
			if calls.Load() != 1 || reasoning.String() != "thinking" {
				t.Errorf("%d requests, reasoning shown %q; want 1 request and the reasoning once", calls.Load(), reasoning.String())
			}
		})
	}
}
