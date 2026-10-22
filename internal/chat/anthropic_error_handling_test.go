package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// partialToolCallStream is an Anthropic stream cut off in the middle of a write_file call's
// arguments, with no content_block_stop, message_delta or message_stop.
const partialToolCallStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"write_file\",\"input\":{}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"a.go\\\",\\\"content\\\":\\\"pack\"}}\n\n"

// TestAnthropicCleanCloseBeforeStopIsRetried checks that a stream the server closes cleanly before
// the stop reason is not taken as a complete reply, so a cut-off tool call never runs, and that it
// is retried when no output has been shown.
func TestAnthropicCleanCloseBeforeStopIsRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, partialToolCallStream)
			return
		}
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+anthropicEndTurn)
	}))
	defer server.Close()

	msg, _, err := anthropicTestClient(server.URL, 2).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if msg.Content != "ok" || len(msg.ToolCalls) != 0 || calls.Load() != 2 {
		t.Errorf("content %q, %d tool calls after %d calls; want ok, no tool call, 2 calls", msg.Content, len(msg.ToolCalls), calls.Load())
	}
}

// TestAnthropicCleanCloseAfterOutputIsError checks that a stream closed before the stop reason,
// after text was shown, is reported as incomplete rather than retried, since a retry would show
// the text again.
func TestAnthropicCleanCloseAfterOutputIsError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
	}))
	defer server.Close()

	_, _, err := anthropicTestClient(server.URL, 2).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if !errors.Is(err, errIncompleteStream) {
		t.Fatalf("err = %v, want errIncompleteStream", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (output was shown)", calls.Load())
	}
}

// TestAnthropicRetryableStreamErrorTypes checks that each error type whose HTTP status is retried
// is retried when the API reports it inside the stream before output, and that other types are
// not.
func TestAnthropicRetryableStreamErrorTypes(t *testing.T) {
	tests := []struct {
		errType   string
		wantCalls int32
	}{
		{"rate_limit_error", 2},
		{"api_error", 2},
		{"timeout_error", 2},
		{"overloaded_error", 2},
		{"invalid_request_error", 1},
		{"billing_error", 1},
	}
	for _, tt := range tests {
		t.Run(tt.errType, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":%q,\"message\":\"m\"}}\n\n", tt.errType)
					return
				}
				fmt.Fprint(w, anthropicEndTurn)
			}))
			defer server.Close()

			_, _, err := anthropicTestClient(server.URL, 2).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
			if calls.Load() != tt.wantCalls {
				t.Errorf("calls = %d, want %d (err %v)", calls.Load(), tt.wantCalls, err)
			}
			if tt.wantCalls == 2 && err != nil {
				t.Errorf("err = %v, want the retry to succeed", err)
			}
			if tt.wantCalls == 1 && (err == nil || !strings.Contains(err.Error(), tt.errType)) {
				t.Errorf("err = %v, want the %s", err, tt.errType)
			}
		})
	}
}

// TestStreamErrorNamesRequestID checks that an error reported inside a stream names the request-id
// response header, and stays a retryableStreamError.
func TestStreamErrorNamesRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req_stream1")
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer server.Close()

	_, _, err := anthropicTestClient(server.URL, 0).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "(request_id req_stream1)") {
		t.Fatalf("err = %v, want it to name the request ID", err)
	}
	if _, ok := errors.AsType[*retryableStreamError](err); !ok {
		t.Errorf("err = %v, want it to wrap a retryableStreamError", err)
	}
}

// TestHTTPErrorBodyShape checks how a non-2xx body is shown: an Anthropic error as its type and
// message with the request ID, from the body or else the request-id header, and any other body
// as it came.
func TestHTTPErrorBodyShape(t *testing.T) {
	tests := []struct {
		name, header, body       string
		wantBody, wantType, want string
	}{
		{
			name:     "anthropic body",
			body:     `{"type":"error","error":{"type":"billing_error","message":"Your credit balance is too low."},"request_id":"req_body"}`,
			wantBody: "billing_error: Your credit balance is too low. (request_id req_body)",
			wantType: "billing_error", want: "req_body",
		},
		{
			name:     "request ID from header",
			header:   "req_hdr",
			body:     `{"type":"error","error":{"type":"permission_error","message":"no access"}}`,
			wantBody: "permission_error: no access (request_id req_hdr)",
			wantType: "permission_error", want: "req_hdr",
		},
		{
			name:     "openai body unchanged",
			body:     `{"error":{"type":"invalid_request_error","message":"bad"}}`,
			wantBody: `{"error":{"type":"invalid_request_error","message":"bad"}}`,
		},
		{
			name:     "html from a proxy",
			header:   "req_html",
			body:     "<html>413 Request Entity Too Large</html>",
			wantBody: "<html>413 Request Entity Too Large</html> (request_id req_html)",
			want:     "req_html",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.header != "" {
					w.Header().Set("request-id", tt.header)
				}
				w.WriteHeader(http.StatusPaymentRequired)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()

			_, _, err := anthropicTestClient(server.URL, 0).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
			se, ok := errors.AsType[*StatusError](err)
			if !ok {
				t.Fatalf("err = %v, want a StatusError", err)
			}
			if se.Body != tt.wantBody || se.Type != tt.wantType || se.RequestID != tt.want {
				t.Errorf("Body %q, Type %q, RequestID %q; want %q, %q, %q", se.Body, se.Type, se.RequestID, tt.wantBody, tt.wantType, tt.want)
			}
			if !strings.HasPrefix(err.Error(), "HTTP 402: ") {
				t.Errorf("err = %q, want the HTTP 402: prefix", err)
			}
		})
	}
}

// TestConflictIsRetried checks that a 409 conflict_error is retried, as the API asks.
func TestConflictIsRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"type":"error","error":{"type":"conflict_error","message":"conflict"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicEndTurn)
	}))
	defer server.Close()

	if _, _, err := anthropicTestClient(server.URL, 2).StreamChat(context.Background(), testRef, nil, nil, nil, nil); err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}
