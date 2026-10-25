package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hmj-agent/v2/internal/config"
)

// TestToolCallProgressFromEachProvider streams a write_file call from each provider and checks
// that the function WithToolCallProgress put in the context hears of it as it is written: when
// it starts, where the provider says so, and as each piece of its arguments arrives.
func TestToolCallProgressFromEachProvider(t *testing.T) {
	sse := func(events ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range events {
				fmt.Fprintf(w, "%s\n\n", ev)
			}
		}
	}
	for _, tc := range []struct {
		api, base string
		handler   http.HandlerFunc
		want      string
	}{
		{config.APIAnthropic, "", sse(
			`event: message_start`+"\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"write_file","input":{}}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
		), "write_file:0 write_file:8 write_file:15"},
		{config.APIResponses, "", sse(
			`event: response.output_item.added`+"\n"+`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"write_file"}}`,
			`event: response.function_call_arguments.delta`+"\n"+`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":"}`,
			`event: response.function_call_arguments.delta`+"\n"+`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"a.go\"}"}`,
			`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"status":"completed"}}`,
		), "write_file:0 write_file:8 write_file:15"},
		{config.APICompletions, "", sse(
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		), "write_file:8 write_file:15"},
		{config.APIGemini, "/v1beta", sse(
			`data: {"event_type":"step.start","index":0,"step":{"id":"fc-1","type":"function_call","name":"write_file","arguments":{}}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"path\":"}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"\"a.go\"}"}}`,
			`data: {"event_type":"step.stop","index":0}`,
			`data: {"event_type":"interaction.completed","interaction":{"status":"requires_action"}}`,
		), "write_file:0 write_file:8 write_file:15"},
	} {
		t.Run(tc.api, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			cfg := config.Config{Providers: map[string]config.Provider{
				"p": {BaseURL: server.URL + tc.base, APIKey: "k", API: tc.api, Models: []config.Model{{ID: "m"}}},
			}}
			var heard []string
			ctx := WithToolCallProgress(context.Background(), func(name string, argBytes int) {
				heard = append(heard, fmt.Sprintf("%s:%d", name, argBytes))
			})
			msg, _, err := NewClient(cfg).StreamChat(ctx, config.ModelRef{Provider: "p", Model: "m"},
				[]Message{{Role: "user", Content: "write it"}}, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(heard, " "); got != tc.want {
				t.Errorf("heard %q, want %q", got, tc.want)
			}
			if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"path":"a.go"}` {
				t.Errorf("tool calls = %+v", msg.ToolCalls)
			}
		})
	}
}
