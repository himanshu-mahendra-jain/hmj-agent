package chat

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestResponsesAccumulatorFinishReason(t *testing.T) {
	cases := []struct {
		name, event, want string
	}{
		{"completed", `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`, "stop"},
		{"max output tokens", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":3,"output_tokens":2}}}`, "length"},
		{"content filter", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`, "incomplete"},
		{"no terminal event", `{"type":"response.output_text.delta","delta":"hi"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newResponsesAccumulator(time.Now())
			if err := acc.handle("", tc.event, nil, nil); err != nil {
				t.Fatalf("handle failed: %v", err)
			}
			_, usage := acc.result()
			if usage.FinishReason != tc.want {
				t.Fatalf("FinishReason = %q, want %q", usage.FinishReason, tc.want)
			}
		})
	}
}

// Argument events and some output_item.done events identify a call only by
// item_id. They must update the call announced for that item rather than
// dropping the arguments or inventing a second call.
func TestResponsesAccumulatorResolvesCallsByItemID(t *testing.T) {
	acc := newResponsesAccumulator(time.Now())
	events := []string{
		`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"read_file"}}`,
		`{"type":"response.output_item.added","item":{"id":"fc_2","type":"function_call","call_id":"call_b","name":"list_files"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"a.txt\"}"}`,
		`{"type":"response.output_item.done","item":{"id":"fc_2","type":"function_call","name":"list_files","arguments":"{\"path\":\".\"}"}}`,
	}
	for _, ev := range events {
		if err := acc.handle("", ev, nil, nil); err != nil {
			t.Fatalf("handle failed: %v", err)
		}
	}

	msg, _ := acc.result()
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %+v", len(msg.ToolCalls), msg.ToolCalls)
	}
	want := map[string]string{"call_a": `{"path":"a.txt"}`, "call_b": `{"path":"."}`}
	for _, tc := range msg.ToolCalls {
		if got := tc.Function.Arguments; got != want[tc.ID] {
			t.Errorf("%s arguments = %q, want %q", tc.ID, got, want[tc.ID])
		}
	}
}

// An "error" event and a "response.failed" end are reported as errors with the API's code and
// message, retried when they are a server error or a rate limit, not taken for an empty reply.
func TestResponsesStreamErrors(t *testing.T) {
	cases := []struct {
		name, event, want string
		retryable         bool
	}{
		{"error event", `{"type":"error","code":"invalid_prompt","message":"Bad prompt.","sequence_number":3}`, "openai error (invalid_prompt): Bad prompt.", false},
		{"failed response", `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"The server had an error."}}}`, "openai error (server_error): The server had an error.", true},
		{"rate limited", `{"type":"error","code":"rate_limit_exceeded","message":"Slow down."}`, "openai error (rate_limit_exceeded): Slow down.", true},
		{"failed without an error", `{"type":"response.failed","response":{"status":"failed"}}`, `openai stream error: {"type":"response.failed"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := newResponsesAccumulator(time.Now()).handle("", tc.event, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, retryable := errors.AsType[*retryableStreamError](err); retryable != tc.retryable {
				t.Errorf("retryable = %v, want %v", retryable, tc.retryable)
			}
		})
	}
}
